package main

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	c "github.com/TwiN/go-color"
	"github.com/kovidgoyal/imaging"
	. "github.com/tp-link-extender/RCCService/Shared"
)

const (
	// render pings carry base64 images, so they're far bigger than scripts
	maxPingBytes = 64 << 20
	maxScriptBytes = 8 << 20

	// the sizes the Site's render images are scaled to
	bodyResolution = 420
	headResolution = 150
)

var (
	// xmlText escapes text going into the SOAP templates, so a < or & in a script or ID can't break (or add to) the envelope
	xmlText = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

	// errTooLarge marks a body over its size limit (413)
	errTooLarge = errors.New("request body too large")

	// errBadCallback marks a callback body that can't be used (400)
	errBadCallback = errors.New("bad callback body")
)

// compressImage decodes a base64 image and scales it to a PNG, resolution pixels square
func compressImage(b64 string, resolution int, compressed *bytes.Buffer) error {
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return fmt.Errorf("failed to decode base64 of image: %w", err)
	}

	srcimg, err := imaging.Decode(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("failed to decode image from data: %w", err)
	}

	// Lanczos my beloved 💖 (change it to something faster idc)
	img := imaging.Resize(srcimg, resolution, resolution, imaging.Lanczos)
	if img == nil {
		return errors.New("failed to create image from data")
	}

	return imaging.Encode(compressed, img, imaging.PNG)
}

// compressRenderImages scales a render's body image, and its head image when there is one
func compressRenderImages(bodyB64 string, headB64 *string) (body, head []byte, err error) {
	var (
		wg sync.WaitGroup
		bodyBuf, headBuf bytes.Buffer
		bodyErr, headErr error
	)

	// the two are compressed concurrently into their own buffers, so their order can't mix
	wg.Go(func() {
		bodyErr = compressImage(bodyB64, bodyResolution, &bodyBuf)
	})

	if headB64 != nil {
		wg.Go(func() {
			headErr = compressImage(*headB64, headResolution, &headBuf)
		})
	}

	wg.Wait()

	if bodyErr != nil {
		return nil, nil, fmt.Errorf("compress body image: %w", bodyErr)
	}

	if headErr != nil {
		return nil, nil, fmt.Errorf("compress head image: %w", headErr)
	}

	return bodyBuf.Bytes(), headBuf.Bytes(), nil
}

// encodeRenderUpdate builds the body the Site expects: the status line, then for Completed
// the big-endian body and head lengths followed by the images
func encodeRenderUpdate(status string, body, head []byte) []byte {
	var buf bytes.Buffer

	buf.WriteString(status)
	buf.WriteByte('\n')

	if status == "Completed" {
		binary.Write(&buf, binary.BigEndian, uint32(len(body)))
		binary.Write(&buf, binary.BigEndian, uint32(len(head)))
		buf.Write(body)
		buf.Write(head)
	}

	return buf.Bytes()
}

// buildRenderUpdate turns RCC's render callback into the Site's update body
func buildRenderUpdate(raw []byte) ([]byte, error) {
	data := strings.Split(string(raw), "\n")

	switch status := data[0]; status {
	case "Rendering":
		return encodeRenderUpdate(status, nil, nil), nil
	case "Completed":
		if len(data) < 2 {
			return nil, fmt.Errorf("%w: completed without a body image", errBadCallback)
		}

		var headB64 *string

		if len(data) >= 3 {
			headB64 = &data[2]
		}

		body, head, err := compressRenderImages(data[1], headB64)
		if err != nil {
			return nil, err
		}

		return encodeRenderUpdate(status, body, head), nil
	}

	return nil, fmt.Errorf("%w: unknown render status %q", errBadCallback, status)
}

// postRenderUpdate reports a render's progress to the Site's render update route
func (o *orbiter) postRenderUpdate(id string, payload []byte) error {
	req, err := http.NewRequest(http.MethodPost, o.cfg.SiteURL+"/api/render/update/"+id, bytes.NewReader(payload))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer "+o.cfg.Key)

	res, err := o.site.Do(req)
	if err != nil {
		return err
	}

	defer res.Body.Close()

	io.Copy(io.Discard, res.Body)

	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("the Site responded %s", res.Status)
	}

	return nil
}

// renderStartRoute starts a render from the Site. The body is the render loadscript, already
// carrying its PING_URL (the loopback callback)
func (o *orbiter) renderStartRoute(w http.ResponseWriter, r *http.Request) {
	id, ok := renderID(w, r)
	if !ok {
		return
	}

	Log(c.InBlue("Received render request for " + id))

	script, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxScriptBytes))
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			writeError(w, http.StatusRequestEntityTooLarge, "render script is larger than 8 MiB")
			return
		}

		writeError(w, http.StatusBadRequest, "failed to read render script: "+err.Error())

		return
	}

	// placeholders are filled in before the script, so a script containing one of them can't
	// be rewritten by a later substitution
	currentTemplate := strings.ReplaceAll(renderTemplate, "_TASK_ID", xmlText.Replace(id))
	currentTemplate = strings.ReplaceAll(currentTemplate, "_EXPIRATION", "30")
	currentTemplate = strings.ReplaceAll(currentTemplate, "_RENDER_SCRIPT", xmlText.Replace(string(script)))

	if err := o.pool.Submit(id, "render", currentTemplate); err != nil {
		Log(c.InRed("Failed to start render " + id + ": " + err.Error()))
		writeError(w, http.StatusInternalServerError, "failed to start render: "+err.Error())

		return
	}

	w.WriteHeader(http.StatusAccepted)
}

// readPingBody reads a callback body, gunzipping it when it's gzipped
func readPingBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	readBody, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPingBytes))
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return nil, errTooLarge
		}

		return nil, fmt.Errorf("read request body: %w", err)
	}

	// if the body is gzipped, unzip it
	if !bytes.HasPrefix(readBody, []byte("\x1f\x8b")) {
		return readBody, nil
	}

	reader, err := gzip.NewReader(bytes.NewReader(readBody))
	if err != nil {
		return nil, fmt.Errorf("%w: create gzip reader: %w", errBadCallback, err)
	}

	// limited after unzipping too, so a small gzip bomb can't expand without bound
	readBody, err = io.ReadAll(io.LimitReader(reader, maxPingBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read gzipped body: %w", errBadCallback, err)
	}

	if len(readBody) > maxPingBytes {
		return nil, errTooLarge
	}

	return readBody, nil
}

// renderPingRoute takes RCC's render callback on the loopback listener and passes it to the Site
func (o *orbiter) renderPingRoute(w http.ResponseWriter, r *http.Request) {
	id, ok := renderID(w, r)
	if !ok {
		return
	}

	raw, err := readPingBody(w, r)
	if errors.Is(err, errTooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	}

	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	payload, err := buildRenderUpdate(raw)
	if errors.Is(err, errBadCallback) {
		Log(c.InRed("Render " + id + " sent a bad callback: " + err.Error()))
		writeError(w, http.StatusBadRequest, err.Error())

		return
	}

	if err != nil {
		Log(c.InRed("Render " + id + ": " + err.Error()))
		writeError(w, http.StatusInternalServerError, err.Error())

		return
	}

	o.pool.freeRenderSlot(id)

	if err := o.postRenderUpdate(id, payload); err != nil {
		Log(c.InRed("Failed to send render " + id + " to the Site: " + err.Error()))
		writeError(w, http.StatusInternalServerError, "failed to report render to the Site: "+err.Error())

		return
	}

	Log(c.InGreen("Render " + id + " reported to the Site"))
	w.WriteHeader(http.StatusOK)
}
