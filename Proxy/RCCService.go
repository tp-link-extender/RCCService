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
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	c "github.com/TwiN/go-color"
	env "github.com/joho/godotenv"
	"github.com/kovidgoyal/imaging"
	. "github.com/tp-link-extender/RCCService/Shared"
)

const exePath = "./RCCService/RCCService.exe"

// resolved at startup from the proxy's CWD; Windows can't resolve a relative exe
// path once cmd.Dir moves the child elsewhere
var absExePath, absDir string

//go:embed render.xml
var renderTemplate string

//go:embed host.xml
var hostTemplate string

//go:embed renew.xml
var renewTemplate string

//go:embed close.xml
var closeTemplate string

var proxyListenerPort = 64990

var client http.Client

func Logr(txt string) {
	fmt.Print("\r", time.Now().Format("2006/01/02, 15:04:05  "), txt) // fmt.Print don't add spaces between args
}

// a single RCCService process, tied to a dedicated port with `-Console <port>`
type rccInstance struct {
	port int

	mu      sync.Mutex
	jobs    map[string]*job
	alive   bool
	started bool

	// held-open write end of the instance's stdin pipe; RCC reads its management
	// input from stdin, and an empty/closed stdin makes it exit cleanly (after
	// running whatever jobs it already had)
	stdinWriter *os.File

	restarts int
}

func newRCCInstance(port int) *rccInstance {
	return &rccInstance{
		port:  port,
		jobs:  make(map[string]*job),
		alive: true,
	}
}

func (i *rccInstance) countJobs() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.jobs)
}

type pool struct {
	maxJobs      int
	basePort     int
	expiration   string // raw template value
	renewSeconds int    // 0 = don't renew (job may have 9999999999 expiration)
	renew        bool

	mu        sync.Mutex
	jobs      map[string]*job
	instances []*rccInstance
	reserved  map[int]struct{}
}

type job struct {
	id       string
	kind     string // "render" or "host"
	instance *rccInstance

	stopRenew chan struct{}
	closeOnce sync.Once
}

var jobPool *pool

// run makes sure an instance's process stays alive until the instance is told to die
func (i *rccInstance) run() {
	rccArgs := []string{absExePath, "-Console", strconv.Itoa(i.port)}
	if runtime.GOOS != "windows" {
		rccArgs = append([]string{"wine"}, rccArgs...)
	}

	for {
		cmd := exec.Command(rccArgs[0], rccArgs[1:]...)

		// run from the executable's own directory, matching a manual launch (content,
		// AppSettings.xml etc. are resolved relative to the CWD)
		cmd.Dir = absDir
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		// stdin is kept open for the process's whole lifetime: a closed/empty stdin
		// makes RCC's management input loop end (and the process exits cleanly,
		// taking every running job with it)
		stdinR, stdinW, err := os.Pipe()
		if err != nil {
			Log(c.InRed(fmt.Sprintf("Failed to create stdin pipe for RCC instance on port %d: %s", i.port, err.Error())))
			time.Sleep(5 * time.Second)
			continue
		}
		cmd.Stdin = stdinR
		i.mu.Lock()
		i.stdinWriter = stdinW
		i.mu.Unlock()
		// DON'T close stdinW - holding it open pins the pipe until the process exits

		i.mu.Lock()
		alive := i.alive
		// if the process crashed, all of its jobs died with it - free them back up
		// (renders can be re-requested by the site, and hosts are culled by the orbiter's trackers)
		lost := make([]*job, 0, len(i.jobs))
		for id := range i.jobs {
			lost = append(lost, i.jobs[id])
			delete(i.jobs, id)
		}
		i.mu.Unlock()

		for _, j := range lost {
			jobPool.deleteJob(j.id)
			if j.stopRenew != nil {
				select {
				case <-j.stopRenew:
				default:
					close(j.stopRenew)
				}
			}
		}

		if !alive {
			return
		}

		i.restarts++
		Log(c.InRed(fmt.Sprintf("RCCService instance on port %d has stopped. Restarting... (#%d)", i.port, i.restarts)))

		// back off on fast failures (a spawn error would otherwise flood restarts)
		if i.restarts > 3 {
			time.Sleep(time.Duration(min(30, i.restarts)) * time.Second)
		}
	}
}

func (i *rccInstance) kill() {
	i.mu.Lock()
	i.alive = false
	i.mu.Unlock()
}

func (p *pool) allocPort() (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for port := p.basePort; port < p.basePort+10000; port++ {
		if _, isReserved := p.reserved[port]; isReserved {
			continue
		}

		// if we're able to bind the port, it's free - RCC should be able to grab it
		conn, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			continue
		}
		conn.Close()

		p.reserved[port] = struct{}{}
		return port, nil
	}
	return 0, errors.New("no available ports")
}

// spawn adds an instance to the pool and starts its process (possibly on a newly allocated port)
func (p *pool) spawn() (*rccInstance, error) {
	port, err := p.allocPort()
	if err != nil {
		return nil, err
	}

	instance := newRCCInstance(port)

	p.mu.Lock()
	p.instances = append(p.instances, instance)
	p.mu.Unlock()

	go instance.run()
	return instance, nil
}

// pickInstance grabs the instance with the fewest active jobs; ok = an instance was found
func (p *pool) pickInstance() (instance *rccInstance, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, ins := range p.instances {
		if !ins.alive {
			continue
		}
		if n := ins.countJobs(); n < p.maxJobs && (instance == nil || n < instance.countJobs()) {
			instance = ins
		}
	}
	return instance, instance != nil
}

func (p *pool) getJob(id string) *job {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.jobs[id]
}

func (p *pool) deleteJob(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.jobs, id)
}

// waitForRCC polls the instance's SOAP port until it responds
func waitForRCC(port int) error {
	for start := time.Now(); time.Since(start) < 60*time.Second; time.Sleep(100 * time.Millisecond) {
		if res, err := http.Get(fmt.Sprintf("http://localhost:%d", port)); err == nil {
			res.Body.Close()
			return nil
		}
	}
	return fmt.Errorf("RCC instance on port %d did not respond within 60 seconds", port)
}

// Submit adds the job to the pool, sends the SOAP request to an instance (spawning
// a new instance if they're all full), and starts lease renewal for host jobs
func (p *pool) Submit(id string, kind string, soap string) error {
	p.mu.Lock()
	if _, exists := p.jobs[id]; exists {
		p.mu.Unlock()
		return errors.New("job " + id + " already exists")
	}
	p.jobs[id] = &job{
		id:        id,
		kind:      kind,
		stopRenew: make(chan struct{}),
	}
	p.mu.Unlock()

	cleanupJob := func() {
		if j := p.getJob(id); j != nil && j.stopRenew != nil {
			select {
			case <-j.stopRenew:
			default:
				close(j.stopRenew)
			}
		}
		p.deleteJob(id)
	}

	instance, ok := p.pickInstance()
	if !ok {
		Log(c.InBlue("All RCCService instances are full, spawning a new one..."))

		var err error
		instance, err = p.spawn()
		if err != nil {
			cleanupJob()
			return fmt.Errorf("spawn new RCCService instance: %w", err)
		}
	}

	j := p.getJob(id)
	j.instance = instance

	instance.mu.Lock()
	instance.jobs[id] = j
	started := instance.started
	instance.mu.Unlock()

	if !started {
		if err := waitForRCC(instance.port); err != nil {
			cleanupJob()
			return err
		}
		instance.mu.Lock()
		instance.started = true
		instance.mu.Unlock()
	}

	req, err := http.NewRequest("POST", fmt.Sprintf("http://localhost:%d", instance.port), strings.NewReader(soap))
	if err != nil {
		cleanupJob()
		return err
	}
	req.Header.Set("Content-Type", "text/xml; charset=utf-8")
	req.Header.Set("SOAPAction", "http://roblox.com/OpenJobEx")

	res, err := client.Do(req)
	if err != nil {
		cleanupJob()
		return fmt.Errorf("send request to RCCService: %w", err)
	}
	defer res.Body.Close()
	response, err := io.ReadAll(res.Body)
	if err != nil {
		cleanupJob()
		return fmt.Errorf("read RCCService response: %w", err)
	}

	// RCC returns SOAP faults for malformed requests instead of proper HTTP errors;
	// don't count these jobs as active
	if strings.Contains(string(response), "SOAP-ENV:Fault") {
		cleanupJob()
		return fmt.Errorf("RCCService returned a SOAP fault: %s", string(response))
	}

	Log(c.InBlue(fmt.Sprintf("[submit] %s (%s) on instance port %d - RCC replied: %s", id, kind, instance.port, strings.TrimSpace(string(response)))))

	Log(c.InGreen(fmt.Sprintf("Job %s (%s) started on instance port %d (dir %s)", id, kind, instance.port, filepath.Dir(exePath))))

	// hosting jobs get their leases renewed, render jobs expire by themselves (30s)
	if kind == "host" && p.renew {
		go p.renewLoop(j)
	}

	return nil
}

// renewLoop keeps a hosting job's lease renewed while the pool holds it
func (p *pool) renewLoop(j *job) {
	interval := time.Duration(p.renewSeconds) * time.Second / 2
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-j.stopRenew:
			return
		case <-ticker.C:
			if j.instance == nil {
				return
			}
			j.instance.mu.Lock()
			alive := j.instance.alive
			_, stillOnInstance := j.instance.jobs[j.id]
			j.instance.mu.Unlock()
			if !alive || !stillOnInstance {
				return
			}

			body := strings.ReplaceAll(renewTemplate, "_TASK_ID", j.id)
			body = strings.ReplaceAll(body, "_RENEW_SECONDS", strconv.Itoa(p.renewSeconds))

			req, err := http.NewRequest("POST", fmt.Sprintf("http://localhost:%d", j.instance.port), strings.NewReader(body))
			if err != nil {
				Log(c.InYellow(fmt.Sprintf("Failed to create lease renewal request for job %s: %s", j.id, err.Error())))
				continue
			}
			req.Header.Set("Content-Type", "text/xml; charset=utf-8")
			req.Header.Set("SOAPAction", "http://roblox.com/RenewLease")

			res, err := client.Do(req)
			if err != nil {
				Log(c.InYellow(fmt.Sprintf("Failed to renew lease of job %s: %s", j.id, err.Error())))
				continue
			}
			response, err := io.ReadAll(res.Body)
			res.Body.Close()
			if err != nil {
				continue
			}

			// a fault here likely means the renew template doesn't fit this RCC build;
			// log it instead of flooding more renewals for nothing
			if strings.Contains(string(response), "SOAP-ENV:Fault") {
				Log(c.InRed(fmt.Sprintf("RCCService rejected lease renewal of job %s: %s; stopping renewals", j.id, string(response))))
				return
			}
		}
	}
}

// CloseJob sends a CloseJobEx request to the job's instance and frees its slot
func (p *pool) CloseJob(id string) {
	j := p.getJob(id)
	if j == nil {
		return
	}

	j.closeOnce.Do(func() {
		if j.stopRenew != nil {
			select {
			case <-j.stopRenew:
			default:
				close(j.stopRenew)
			}
		}

		instance := j.instance
		if instance != nil {
			// this RCC build only implements CloseJob (CloseJobEx doesn't exist on it)
			method := "CloseJob"
			body := strings.ReplaceAll(strings.ReplaceAll(closeTemplate, "_TASK_ID", id), "CloseJobEx", method)

			req, err := http.NewRequest("POST", fmt.Sprintf("http://localhost:%d", instance.port), strings.NewReader(body))
			if err != nil {
				Log(c.InRed(fmt.Sprintf("Failed to create close request for job %s: %s", id, err.Error())))
			} else {
				req.Header.Set("Content-Type", "text/xml; charset=utf-8")
				req.Header.Set("SOAPAction", "http://roblox.com/"+method)

				res, err := client.Do(req)
				if err != nil {
					Log(c.InRed(fmt.Sprintf("Failed to close job %s: %s", id, err.Error())))
				} else {
					response, err := io.ReadAll(res.Body)
					res.Body.Close()
					if err != nil {
						Log(c.InRed(fmt.Sprintf("Failed to read close response for job %s: %s", id, err.Error())))
					} else if strings.Contains(string(response), "SOAP-ENV:Fault") {
						Log(c.InRed(fmt.Sprintf("RCCService rejected close of job %s: %s", id, string(response))))
					}
				}
			}

			instance.mu.Lock()
			delete(instance.jobs, id)
			instance.mu.Unlock()
		}

		p.deleteJob(id)
	})
}

// freeRenderSlot lightly frees a render job's slot. Host jobs tracking is the orbiter's
// problem, and closing one properly requires CloseJobEx (pool.CloseJob)
func (p *pool) freeRenderSlot(id string) {
	j := p.getJob(id)
	if j == nil || j.kind != "render" {
		return
	}

	if j.instance != nil {
		j.instance.mu.Lock()
		delete(j.instance.jobs, id)
		j.instance.mu.Unlock()
	}
	p.deleteJob(id)
}

func Compress(b64 string, resolution int, name string, compressed *bytes.Buffer) error {
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return fmt.Errorf("failed to decode base64 of image: %w", err)
	}

	srcimg, err := imaging.Decode(strings.NewReader(string(data)))
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

func readPingBody(r *http.Request) ([]byte, error) {
	readBody, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("read request body: %w", err)
	}

	// if the body is gzipped, unzip it
	if strings.HasPrefix(string(readBody), "\x1f\x8b") {
		reader, err := gzip.NewReader(bytes.NewReader(readBody))
		if err != nil {
			return nil, fmt.Errorf("create gzip reader: %w", err)
		}
		readBody, err = io.ReadAll(reader)
		if err != nil {
			return nil, fmt.Errorf("read gzipped request body: %w", err)
		}
	}

	return readBody, nil
}

// ID route starts a render, coming from the Site
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

	script := strings.ReplaceAll(string(loadScript), "_PING_URL", "http://127.0.0.1:64990/ping")

	id := r.PathValue("id")
	currentTemplate := strings.ReplaceAll(renderTemplate, "_TASK_ID", id)
	currentTemplate = strings.ReplaceAll(currentTemplate, "_EXPIRATION", "30")
	currentTemplate = strings.ReplaceAll(currentTemplate, "_RENDER_SCRIPT", script)

	Log(c.InBlue("Sending request to render " + id))
	if err := jobPool.Submit(id, "render", currentTemplate); err != nil {
		Log(c.InRed("Failed to start render: " + err.Error()))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

// Ping callback comes from RCCService when a render status changes
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

	readBody, err := readPingBody(r)
	if err != nil {
		Log(c.InRed("Failed to read ping body: " + err.Error()))
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	data := strings.Split(string(readBody), "\n")
	status := data[0]

	var encoded bytes.Buffer
	encoded.WriteString(status)
	encoded.WriteByte('\n')

	id := r.PathValue("id")

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

	jobPool.freeRenderSlot(id)

	// Send to server as binary
	req, err := http.NewRequest("POST", os.Getenv("ENDPOINT")+"/"+id, &encoded)
	if err != nil {
		Log(c.InRed("Failed to create request to send render data to server: " + err.Error()))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// We (still) have to lie about the contentType to avoid being nuked by CORS from the website
	req.Header.Set("Content-Type", "text/json")
	req.Header.Set("Authorization", "Bearer "+os.Getenv("RCC_KEY"))

	res, err := client.Do(req)
	if err != nil {
		Log(c.InRed("Failed to send render data to server: " + err.Error()))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	res.Body.Close()

	if res.StatusCode != http.StatusOK {
		Log(c.InRed(fmt.Sprintf("Server (%s) responded with status code %d", req.URL.String(), res.StatusCode)))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

// hostping callback comes from hosted gameservers, padded by the proxy's host epilogue.
// Statuses are relayed to the Orbiter's hoststatus route
func hostpingIdRoute(w http.ResponseWriter, r *http.Request) {
	Log(c.InBlue("Received host ping callback"))
	ips := r.RemoteAddr[:strings.LastIndex(r.RemoteAddr, ":")]
	ips = strings.Trim(ips, "[]")
	if ip := net.ParseIP(ips); !net.IPv6loopback.Equal(ip) && !net.IPv4(127, 0, 0, 1).Equal(ip) {
		Log(c.InRed("IP " + ips + " is not allowed! (hostping)"))
		w.WriteHeader(http.StatusForbidden)
		return
	}

	readBody, err := readPingBody(r)
	if err != nil {
		Log(c.InRed("Failed to read host ping body: " + err.Error()))
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	id := r.PathValue("id")
	Log(c.InGreen(fmt.Sprintf("Host job %s pinged: %s", id, strings.SplitN(string(readBody), "\n", 2)[0])))

	endpoint := os.Getenv("HOST_ENDPOINT")
	if endpoint == "" {
		return
	}

	req, err := http.NewRequest("POST", endpoint+"/"+id, bytes.NewReader(readBody))
	if err != nil {
		Log(c.InRed("Failed to create request to send host status to orbiter: " + err.Error()))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "text/json")
	req.Header.Set("Authorization", "Bearer "+os.Getenv("RCC_KEY"))

	res, err := client.Do(req)
	if err != nil {
		Log(c.InRed("Failed to send host status to orbiter: " + err.Error()))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		Log(c.InRed(fmt.Sprintf("Orbiter (%s) responded with status code %d", req.URL.String(), res.StatusCode)))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	Log(c.InGreen("Relayed host status of " + id + " to " + req.URL.String()))
}

// hostRoute starts a hosted gameserver job on an RCC instance, coming from the Orbiter.
// The Orbiter sends the full host script (from the Site) as the request body
func hostRoute(w http.ResponseWriter, r *http.Request) {
	Log(c.InBlue("Received host request"))
	if !checkAuth(r) {
		Log(c.InRed("Request to host route is not authenticated!"))
		w.WriteHeader(http.StatusForbidden)
		return
	}

	loadScript, err := io.ReadAll(r.Body)
	if err != nil {
		Log(c.InRed("Failed to read host script: " + err.Error()))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	id := r.PathValue("id")

	// prepend our own status reporting to the script: OpenJobEx jobs don't pump a
	// scheduler (see above), so nothing after RunService:Run() would ever execute
	epilogue := strings.ReplaceAll(hostEpilogue, "_PING_URL", "http://127.0.0.1:"+strconv.Itoa(proxyListenerPort)+"/hostping/"+id)
	script := epilogue + "\n\n" + string(loadScript)

	firstLine := strings.SplitN(string(loadScript), "\n", 2)[0]
	Log(c.InBlue(fmt.Sprintf("[host] %s - host script received (%d chars), first line: %q", id, len(script), firstLine)))

	currentTemplate := strings.ReplaceAll(hostTemplate, "_TASK_ID", id)
	currentTemplate = strings.ReplaceAll(currentTemplate, "_EXPIRATION", os.Getenv("HOST_EXPIRATION"))
	currentTemplate = strings.ReplaceAll(currentTemplate, "_HOST_SCRIPT", script+"  ")

	if err := jobPool.Submit(id, "host", currentTemplate); err != nil {
		Log(c.InRed("Failed to start host job: " + err.Error()))
		http.Error(w, "Failed to start host job: "+err.Error(), http.StatusInternalServerError)
		return
	}
}

// closeHostRoute stops lease renewal and tells RCC to close a hosted job
func closeHostRoute(w http.ResponseWriter, r *http.Request) {
	Log(c.InBlue("Received host close request"))
	if !checkAuth(r) {
		Log(c.InRed("Request to close route is not authenticated!"))
		w.WriteHeader(http.StatusForbidden)
		return
	}

	id := r.PathValue("id")
	jobPool.CloseJob(id)

	Log(c.InYellow("Host job " + id + " closed"))
}

func checkAuth(r *http.Request) bool {
	key := os.Getenv("PROXY_KEY")
	if key == "" {
		// backwards compat: fall back to the render route's IP whitelisting
		ip := r.RemoteAddr[:strings.LastIndex(r.RemoteAddr, ":")]
		ip = strings.Trim(ip, "[]")
		return net.ParseIP(ip).IsLoopback() || ip == os.Getenv("IP")
	}

	if r.Header.Get("Authorization") != "Bearer "+key {
		return false
	}
	return true
}

// PREpended by the proxy to every host script sent to RCC. IMPORTANT: OpenJobEx script
// environments on this RCC build don't pump a scheduler - wait() and delay() never
// resume - so all reporting here is synchronous at script start, or fires off of
// engine events once RunService:Run() pumps the heartbeat in the rest of the script
var hostEpilogue = `print "[Proxy]: Reporting to proxy..."
local Players = game:GetService "Players"

pcall(function()
	game:HttpPost("_PING_URL", "Loaded", true, "text/json")
end)

Players.PlayerAdded:connect(function(player)
	pcall(function()
		game:HttpPost("_PING_URL", "PlayerAdded\n" .. tostring(player.userId), true, "text/json")
		print(("[Proxy]: Player %d added and reported"):format(player.userId))
	end)
end)

Players.PlayerRemoving:connect(function(player)
	pcall(function()
		game:HttpPost("_PING_URL", "PlayersLeft\n" .. tostring(player.userId), true, "text/json")
		print(("[Proxy]: Player %d left and reported"):format(player.userId))
	end)
end)`

func main() {
	Log(c.InYellow("Loading environment variables..."))
	Fatal(env.Load(".env"), "Failed to load environment variables. Please place them in a .env file in the current directory.")

	maxJobs, err := strconv.Atoi(os.Getenv("MAX_JOBS_PER_RCC"))
	Fatal(err, "MAX_JOBS_PER_RCC must be an integer")
	basePort, err := strconv.Atoi(os.Getenv("RCC_BASE_PORT"))
	Fatal(err, "RCC_BASE_PORT must be an integer")

	// HOW EXPIRATION WORKS:
	// - HOST_EXPIRATION=300 -> jobs last 300s; leases are renewed with RenewLease every
	//   150s (strategy 1: self-healing, jobs die if the proxy stops renewing them)
	// - HOST_EXPIRATION=0   -> jobs last practically forever (9999999999s) and are
	//   explicitly closed with CloseJobEx when the Orbiter asks for it (strategy 2)
	expiration := os.Getenv("HOST_EXPIRATION")
	if expiration == "0" {
		expiration = "9999999999"
	}
	renewSeconds, err := strconv.Atoi(os.Getenv("HOST_EXPIRATION"))
	Fatal(err, "HOST_EXPIRATION must be an integer (0 = no renewal, close jobs explicitly)")

	// resolve the RCC executable locations once; the relative const is only a
	// documented default otherwise
	absExePath, err = filepath.Abs(exePath)
	Fatal(err, "Failed to resolve RCCService path.")
	absDir = filepath.Dir(absExePath)

	// reserve the listener before spawning RCC instances, so a bind failure doesn't
	// leave orphaned instances behind
	jobPool = &pool{
		maxJobs:      maxJobs,
		basePort:     basePort,
		expiration:   expiration,
		renew:        renewSeconds > 0,
		renewSeconds: renewSeconds,
		jobs:         make(map[string]*job),
		reserved: map[int]struct{}{
			proxyListenerPort: {}, // our own listener
			64991:             {}, // Orbiter
			64992:             {}, // Orbiter public status
		},
	}

	listener, err := net.Listen("tcp", ":"+strconv.Itoa(proxyListenerPort))
	if err != nil {
		Fatal(err, fmt.Sprintf("Failed to bind RCCService proxy port %d.", proxyListenerPort))
	}
	defer listener.Close()

	Log(c.InPurple("Starting RCCService..."))
	instance, err := jobPool.spawn()
	Fatal(err, "Failed to spawn initial RCCService instance.")

	Logr(c.InPurple("Waiting for RCCService to start..."))
	if err := waitForRCC(instance.port); err != nil {
		Fatal(err, "RCCService took too long to start.")
	}
	instance.started = true

	fmt.Println()
	Log(c.InPurple("Starting server..."))

	http.HandleFunc("POST /{id}", idRoute)
	http.HandleFunc("POST /ping/{id}", pingIdRoute)
	http.HandleFunc("POST /host/{id}", hostRoute)
	http.HandleFunc("POST /hostping/{id}", hostpingIdRoute)
	http.HandleFunc("DELETE /host/{id}", closeHostRoute)

	Log(c.InGreen(fmt.Sprintf("~ RCCService proxy is up on port %d ~", proxyListenerPort)))
	Log(c.InGreen("Send a POST request to /{your task id} with the render script as the body to start a render"))
	Log(c.InGreen("Send a POST request to /host/{your game id} with the host script as the body to host a gameserver (RCC-side)"))
	if err := http.Serve(listener, nil); err != nil {
		Log(c.InRed("Failed to start RCCService proxy: " + err.Error()))
		os.Exit(1)
	}
}
