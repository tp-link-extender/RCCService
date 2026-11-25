package main

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBuildRenderUpdateRejectsBadCallbacks(t *testing.T) {
	cases := map[string]string{"completed without a body": "Completed", "unknown status": "Exploded"}

	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := buildRenderUpdate([]byte(raw)); !errors.Is(err, errBadCallback) {
				t.Fatalf("err = %v, want errBadCallback", err)
			}
		})
	}
}

func TestBuildRenderUpdateRendering(t *testing.T) {
	got, err := buildRenderUpdate([]byte("Rendering"))
	if err != nil {
		t.Fatal(err)
	}

	if string(got) != "Rendering\n" {
		t.Errorf("got %q", got)
	}
}

func TestEncodeRenderUpdateCompletedLayout(t *testing.T) {
	const prefix = "Completed\n"

	body := []byte("bodyimage")
	head := []byte("head")
	got := encodeRenderUpdate("Completed", body, head)

	if !bytes.HasPrefix(got, []byte(prefix)) {
		t.Fatalf("missing status line: %q", got)
	}

	rest := got[len(prefix):]

	if n := binary.BigEndian.Uint32(rest[0:4]); n != uint32(len(body)) {
		t.Errorf("body length = %d, want %d", n, len(body))
	}

	if n := binary.BigEndian.Uint32(rest[4:8]); n != uint32(len(head)) {
		t.Errorf("head length = %d, want %d", n, len(head))
	}

	if !bytes.Equal(rest[8:8+len(body)], body) || !bytes.Equal(rest[8+len(body):], head) {
		t.Errorf("images are out of order: %q", rest[8:])
	}
}

func TestEncodeRenderUpdateRendering(t *testing.T) {
	if got := encodeRenderUpdate("Rendering", nil, nil); string(got) != "Rendering\n" {
		t.Errorf("got %q, want the status line alone", got)
	}
}

func TestPostRenderUpdateContract(t *testing.T) {
	var (
		gotPath, gotType, gotAuth string
		gotBody []byte
	)

	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotType = r.Header.Get("Content-Type")
		gotAuth = r.Header.Get("Authorization")
		gotBody = make([]byte, r.ContentLength)

		r.Body.Read(gotBody)
		w.WriteHeader(http.StatusOK)
	}))

	defer site.Close()

	o := newOrbiter(config{
		Key: "0123456789abcdef-long",
		SiteURL: site.URL,
	}, "v")

	if err := o.postRenderUpdate("render-1", []byte("Rendering\n")); err != nil {
		t.Fatal(err)
	}

	if gotPath != "/api/render/update/render-1" {
		t.Errorf("path = %q", gotPath)
	}

	if gotType != "application/octet-stream" {
		t.Errorf("content type = %q", gotType)
	}

	if gotAuth != "Bearer 0123456789abcdef-long" {
		t.Errorf("authorization = %q", gotAuth)
	}

	if string(gotBody) != "Rendering\n" {
		t.Errorf("body = %q", gotBody)
	}
}

func TestPostRenderUpdateFailsOnNon200(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))

	defer site.Close()

	o := newOrbiter(config{
		Key: "0123456789abcdef-long",
		SiteURL: site.URL,
	}, "v")

	if err := o.postRenderUpdate("render-1", []byte("Rendering\n")); err == nil {
		t.Fatal("expected an error for a non-200 from the Site")
	}
}

func TestReadPingBodyPlainAndGzipped(t *testing.T) {
	const text = "Rendering"

	plain := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/ping/x", strings.NewReader(text))
	got, err := readPingBody(plain, req)

	if err != nil || string(got) != text {
		t.Fatalf("plain = (%q, %v)", got, err)
	}

	var zipped bytes.Buffer

	zw := gzip.NewWriter(&zipped)

	zw.Write([]byte(text))
	zw.Close()

	req = httptest.NewRequest(http.MethodPost, "/ping/x", &zipped)
	got, err = readPingBody(httptest.NewRecorder(), req)

	if err != nil || string(got) != text {
		t.Fatalf("gzipped = (%q, %v)", got, err)
	}
}

// a gzip bomb: a few kilobytes that expand past the limit
func TestReadPingBodyStopsGzipBombs(t *testing.T) {
	var zipped bytes.Buffer

	zw := gzip.NewWriter(&zipped)
	zeros := make([]byte, 1<<20)

	for range 65 {
		zw.Write(zeros)
	}

	zw.Close()

	req := httptest.NewRequest(http.MethodPost, "/ping/x", &zipped)

	if _, err := readPingBody(httptest.NewRecorder(), req); !errors.Is(err, errTooLarge) {
		t.Fatalf("err = %v, want errTooLarge", err)
	}
}

func TestXMLTextEscapes(t *testing.T) {
	const want string = `a &lt; b &amp;&amp; c &gt; d`

	if got := xmlText.Replace(`a < b && c > d`); got != want {
		t.Errorf("xmlText = %q, want %q", got, want)
	}
}

// pngB64 is a small solid PNG, base64 encoded the way RCC sends render images
func pngB64(t *testing.T, w, h int) string {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, w, h))

	for x := range w {
		for y := range h {
			img.Set(x, y, color.RGBA{
				R: uint8(x),
				G: uint8(y),
				B: 128,
				A: 255,
			})
		}
	}

	var buf bytes.Buffer

	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}

	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestBuildRenderUpdateCompletedScalesImages(t *testing.T) {
	const prefix = "Completed\n"

	raw := []byte("Completed\n" + pngB64(t, 16, 16) + "\n" + pngB64(t, 16, 16))

	got, err := buildRenderUpdate(raw)
	if err != nil {
		t.Fatalf("completed render rejected: %v", err)
	}

	rest := got[len(prefix):]
	bodyLen := binary.BigEndian.Uint32(rest[0:4])
	headLen := binary.BigEndian.Uint32(rest[4:8])

	if bodyLen == 0 || headLen == 0 {
		t.Fatalf("lengths = (%d, %d), want both images present", bodyLen, headLen)
	}

	images := rest[8:]
	if uint32(len(images)) != bodyLen+headLen {
		t.Fatalf("image bytes = %d, want %d", len(images), bodyLen+headLen)
	}

	for _, img := range [][]byte{images[:bodyLen], images[bodyLen:]} {
		decoded, err := png.Decode(bytes.NewReader(img))
		if err != nil {
			t.Fatalf("image is not a PNG: %v", err)
		}

		_ = decoded
	}
}

func TestBuildRenderUpdateCompletedWithoutHead(t *testing.T) {
	got, err := buildRenderUpdate([]byte("Completed\n" + pngB64(t, 8, 8)))
	if err != nil {
		t.Fatal(err)
	}

	if headLen := binary.BigEndian.Uint32(got[len("Completed\n")+4:]); headLen != 0 {
		t.Errorf("head length = %d, want 0 when the callback has no head", headLen)
	}
}
