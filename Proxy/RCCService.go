// "GO?
// WHY THE FUCK
// IS THIS IN GO?
// ARE YOU STUPID
// ???"
// - taskmanager, 9 January 2024

// cope harder
// don't forget da .env

package main

import (
	"bytes"
	"compress/gzip"
	"embed"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	c "github.com/TwiN/go-color"
	"github.com/disintegration/imaging"
	env "github.com/joho/godotenv"
)

var (
	_      embed.FS // force embed import hack
	client http.Client
	//go:embed soap.xml
	template string
)

func Log(txt string) {
	// I HATE GO DATE FORMATTING!!! I HATE GO DATE FORMATTING!!!
	fmt.Println(time.Now().Format("2006/01/02, 15:04:05 "), txt)
}

func Logr(txt string) {
	fmt.Print("\r", time.Now().Format("2006/01/02, 15:04:05  "), txt) // fmt.Print don't add spaces between args
}

var cmd *exec.Cmd

func Fatal(err error, txt string) {
	// so that I don't have to write this every time
	if err == nil {
		return
	}
	fmt.Println(err)
	Log(c.InRed(txt))
	if cmd != nil && cmd.Process != nil && cmd.Process.Pid != 0 {
		Log(c.InRed("Killing RCCService..."))
		cmd.Process.Kill()
	}
	os.Exit(1)
}

func StartRCC() {
	_, err := os.Stat("./RCCService/RCCService.exe")
	Fatal(err, "RCCService.exe not found! Please place the RCCService folder in the current directory.")
	for {
		if runtime.GOOS == "windows" {
			cmd = exec.Command("./RCCService/RCCService.exe", "-Console")
		} else { // lel
			cmd = exec.Command("wine", "./RCCService/RCCService.exe", "-Console")
			// cmd.Env = append(cmd.Env, "DISPLAY=:0")
			// cmd.Env = append(cmd.Env, "WINEPREFIX=/home/heliodex/prefix32")
			// cmd.Env = append(cmd.Env, "WINEARCH=win32")
		}
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Run()
		fmt.Println(cmd.Err)
		Log(c.InRed("RCCService has stopped. Restarting..."))
	}
}

func TestRCCStarted(loaded *bool, t int) {
	Logr(c.InPurple(fmt.Sprintf("Waiting for RCCService to start... (%ds)", t)))
	if _, err := http.Get("http://localhost:64989"); err == nil {
		*loaded = true
	}
}

func Compress(b64 string, resolution int, name string, compressed *bytes.Buffer) error {
	// Log("Base64 is of length " + fmt.Sprint(len(b64)) + " for " + name)
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return fmt.Errorf("Failed to decode base64 of image: %w", err)
	}
	// Log("Decoded base64 is of length " + fmt.Sprint(len(data)))

	srcimg, err := imaging.Decode(strings.NewReader(string(data)))
	if err != nil {
		return fmt.Errorf("Failed to decode image from data: %w", err)
	}

	// Lanczos my beloved 💖 (change it to something faster idc)
	img := imaging.Resize(srcimg, resolution, resolution, imaging.Lanczos)
	if img == nil {
		return errors.New("Failed to create image from data")
	}

	// Log("Compressed image is of length " + fmt.Sprint(compressed.Len()))
	return imaging.Encode(compressed, img, imaging.PNG)
}

func idRoute(w http.ResponseWriter, r *http.Request) {
	Log(c.InBlue("Received render request"))
	// remove port from IP (can't just split by ":" because of IPv6)
	if ip := r.RemoteAddr[:strings.LastIndex(r.RemoteAddr, ":")]; ip != os.Getenv("IP") && ip != "[::1]" {
		Log(c.InRed("IP " + ip + " is not allowed! (render)"))
		w.WriteHeader(http.StatusForbidden)
		return
	}

	loadScript, err := io.ReadAll(r.Body)
	if err != nil {
		Log(c.InRed("Failed to read render script: " + err.Error()))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	script := strings.ReplaceAll(string(loadScript), "_PING_URL", "http://localhost:64990/ping")

	id := r.PathValue("id")
	currentTemplate := strings.ReplaceAll(template, "_TASK_ID", id)
	currentTemplate = strings.ReplaceAll(currentTemplate, "_RENDER_SCRIPT", script)

	req, err := http.NewRequest("POST", "http://localhost:64989", strings.NewReader(currentTemplate))
	if err != nil {
		Log(c.InRed("Failed to create request: " + err.Error()))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	req.Header.Set("Content-Type", "text/xml; charset=utf-8")
	req.Header.Set("SOAPAction", "http://roblox.com/OpenJobEx")

	Log(c.InBlue("Sending request to render " + id))
	res, err := client.Do(req)
	if err != nil {
		Log(c.InRed("Failed to send request to RCCService: " + err.Error()))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		Log(c.InRed("Failed to read response from RCCService: " + err.Error()))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	fmt.Println(string(body))

	Log(c.InGreen("Render " + id + " started"))

	w.WriteHeader(http.StatusOK)
}

func pingIdRoute(w http.ResponseWriter, r *http.Request) {
	Log(c.InBlue("Received ping callback"))
	// remove port from IP (can't just split by ":" because of IPv6)
	ips := r.RemoteAddr[:strings.LastIndex(r.RemoteAddr, ":")]
	ips = strings.Trim(ips, "[]") // remove brackets from IPv6
	if ip := net.ParseIP(ips); !net.IPv6loopback.Equal(ip) && !net.IPv4(127, 0, 0, 1).Equal(ip) {
		Log(c.InRed("IP " + ips + " is not allowed! (ping)"))
		w.WriteHeader(http.StatusForbidden)
		return
	}

	apiKey := r.URL.Query().Get("apiKey")
	id := r.PathValue("id")

	readBody, err := io.ReadAll(r.Body)
	if err != nil {
		Log(c.InRed("Failed to read request body: " + err.Error()))
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// if the body is gzipped, unzip it
	if strings.HasPrefix(string(readBody), "\x1f\x8b") {
		reader, err := gzip.NewReader(strings.NewReader(string(readBody)))
		if err != nil {
			Log(c.InRed("Failed to create gzip reader: " + err.Error()))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		readBody, err = io.ReadAll(reader)
		if err != nil {
			Log(c.InRed("Failed to read gzipped request body: " + err.Error()))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	}

	data := strings.Split(string(readBody), "\n")
	status := data[0]

	var encoded bytes.Buffer
	encoded.WriteString(status)
	encoded.WriteByte('\n')

	switch status {
	case "Rendering":
		Log(c.InGreen("Render " + id + " is rendering"))
	case "Completed":
		var wg sync.WaitGroup

		// Could result in a random order if appending to an array instead
		var body, head bytes.Buffer
		var bodyErr, headErr error
		wg.Go(func() {
			bodyErr = Compress(data[1], 420, "body", &body)
		})
		if len(data) == 3 {
			wg.Go(func() {
				headErr = Compress(data[2], 150, "head", &head)
			})
		}
		wg.Wait()

		if bodyErr != nil {
			Log(c.InRed("Failed to compress body image: " + bodyErr.Error()))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if headErr != nil {
			Log(c.InRed("Failed to compress head image: " + headErr.Error()))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		binary.Write(&encoded, binary.BigEndian, uint32(body.Len()))
		binary.Write(&encoded, binary.BigEndian, uint32(head.Len()))
		encoded.Write(body.Bytes())
		encoded.Write(head.Bytes())

		Log(c.InGreen("Render " + id + " is complete"))
	}

	// Send to server as binary
	endpoint := fmt.Sprintf("%s/%s?apiKey=%s", os.Getenv("ENDPOINT"), id, apiKey)
	// We (still) have to lie about the contentType to avoid being nuked by CORS from the website
	if _, err = http.Post(endpoint, "text/json", &encoded); err != nil {
		Log(c.InRed("Failed to send render data to server: " + err.Error()))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func main() {
	Log(c.InYellow("Loading environment variables..."))
	Fatal(env.Load(".env"), "Failed to load environment variables. Please place them in a .env file in the current directory.")

	Log(c.InPurple("Starting RCCService..."))
	go StartRCC()

	loaded := false
	// goroutines mean it'll actually do it every 100ms
	for startTime := time.Now(); !loaded; time.Sleep(100 * time.Millisecond) {
		go TestRCCStarted(&loaded, int(time.Since(startTime).Seconds()))
	}

	fmt.Println()
	Log(c.InPurple("Starting server..."))

	http.HandleFunc("POST /{id}", idRoute)
	http.HandleFunc("POST /ping/{id}", pingIdRoute)

	Log(c.InGreen("~ RCCService proxy is up on port 64990 ~"))
	Log(c.InGreen("Send a POST request to /{your task id} with the render script as the body to start a render"))
	http.ListenAndServe(":64990", nil)
}
