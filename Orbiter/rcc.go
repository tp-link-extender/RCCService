package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	c "github.com/TwiN/go-color"
	. "github.com/tp-link-extender/RCCService/Shared"
)

const (
	exePath = "./RCCService/RCCService.exe"

	// how many ports above the base a pool may use for RCC instances
	portWindow = 10000

	// RCC instances need a while to start listening on their SOAP port
	rccStartTimeout = 60 * time.Second

	// lease renewal is sent with this much time left before a job expires
	renewAt = 2
)

type (
	// a single RCCService process, tied to a dedicated port with `-Console <port>`
	rccInstance struct {
		port int

		// dedicated workload: "render" or "host"; instances never change kind
		kind string

		mu sync.Mutex
		jobs map[string]*job
		alive, started bool

		// process tracking for the detached spawn mode (used by idle shutdown)
		pid int
		child *exec.Cmd
		restarts int
	}

	// pool owns the RCC instances and the jobs running on them
	pool struct {
		maxJobs, basePort int
		expiration string // the template value; "9999999999" for jobs that are closed explicitly
		renewSeconds int
		renew bool
		spawnMode string
		client *http.Client

		mu sync.Mutex
		jobs map[string]*job
		instances []*rccInstance
		reserved map[int]struct{}
	}

	job struct {
		id, kind string // "render" or "host"
		instance *rccInstance // guarded by pool.mu
		stopRenew chan struct{}
		stopOnce, closeOnce sync.Once
	}
)

// Logr writes a line that the next Log overwrites, for progress that updates in place
func Logr(txt string) {
	fmt.Print("\r", time.Now().Format("2006/01/02, 15:04:05  "), txt)
}

// newPool builds the job pool. reserved are ports the Orbiter itself listens on
func newPool(cfg config, reserved ...int) *pool {
	expiration := strconv.Itoa(cfg.HostExpiration)
	if cfg.HostExpiration == 0 {
		expiration = "9999999999"
	}

	p := &pool{
		maxJobs: cfg.MaxJobsPerRCC,
		basePort: cfg.RCCBasePort,
		expiration: expiration,
		renewSeconds: cfg.HostExpiration,
		renew: cfg.HostExpiration > 0,
		spawnMode: cfg.RCCSpawn,
		client: &http.Client{Timeout: rccStartTimeout},
		jobs: make(map[string]*job),
		reserved: make(map[int]struct{}),
	}

	for _, port := range reserved {
		p.reserved[port] = struct{}{}
	}

	return p
}

// start spawns the always-on render instance and waits until it answers
func (p *pool) start() error {
	instance, err := p.spawn("render")
	if err != nil {
		return fmt.Errorf("spawn initial RCCService instance: %w", err)
	}

	Logr(c.InPurple("Waiting for RCCService to start..."))

	if err := waitForRCC(instance.port); err != nil {
		return err
	}

	instance.mu.Lock()

	instance.started = true

	instance.mu.Unlock()

	return nil
}

func (p *pool) allocPort() (int, error) {
	p.mu.Lock()

	defer p.mu.Unlock()

	for port := p.basePort; port < p.basePort+portWindow; port++ {
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

// release forgets an instance that has stopped for good, so its port can be used again
func (p *pool) release(i *rccInstance) {
	p.mu.Lock()

	defer p.mu.Unlock()

	delete(p.reserved, i.port)

	p.instances = slices.DeleteFunc(p.instances, func(x *rccInstance) bool {
		return x == i
	})
}

// spawn adds an instance to the pool and starts its process on a newly allocated port
func (p *pool) spawn(kind string) (*rccInstance, error) {
	port, err := p.allocPort()
	if err != nil {
		return nil, err
	}

	instance := &rccInstance{
		port: port,
		kind: kind,
		jobs: make(map[string]*job),
		alive: true,
	}

	p.mu.Lock()

	p.instances = append(p.instances, instance)

	p.mu.Unlock()

	go func() {
		p.run(instance)
		p.release(instance)
	}()

	return instance, nil
}

// pickInstance grabs the same-kind instance with the fewest active jobs; ok = one was found.
// Render and host instances never share jobs
func (p *pool) pickInstance(kind string) (instance *rccInstance, ok bool) {
	p.mu.Lock()

	defer p.mu.Unlock()

	fewest := p.maxJobs

	for _, ins := range p.instances {
		if ins.kind != kind {
			continue
		}

		ins.mu.Lock()

		alive := ins.alive
		n := len(ins.jobs)

		ins.mu.Unlock()

		if alive && n < fewest {
			instance, fewest = ins, n
		}
	}

	return instance, instance != nil
}

// run keeps an instance's process alive until the instance is told to die
func (p *pool) run(i *rccInstance) {
	for {
		switch p.spawnMode {
		case modeDetached:
			if err := spawnDetached(i); err != nil {
				Log(c.InRed(fmt.Sprintf("Failed to spawn RCCService on port %d: %s", i.port, err.Error())))
				time.Sleep(10 * time.Second)

				continue
			}

			i.trackPid()

			for spawnAlive(i) {
				time.Sleep(3 * time.Second)
			}
		default:
			argv := rccArgv(i.port)
			cmd := exec.Command(argv[0], argv[1:]...)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr

			i.mu.Lock()

			i.child = cmd

			i.mu.Unlock()

			if err := cmd.Run(); err != nil {
				fmt.Println(err)
			}
		}

		i.mu.Lock()

		alive := i.alive

		// if the process stopped, all of its jobs died with it: free them up
		// (renders can be re-requested by the Site, and hosts are culled by the gameserver trackers)
		lost := make([]*job, 0, len(i.jobs))

		for id := range i.jobs {
			lost = append(lost, i.jobs[id])

			delete(i.jobs, id)
		}

		i.mu.Unlock()

		for _, j := range lost {
			p.removeJob(j)
			j.stopRenewing()
		}

		if !alive {
			return
		}

		// only render instances restart themselves; a host instance that stopped is finished,
		// so mark it dead and let the pool forget it
		if i.kind != "render" {
			i.mu.Lock()

			i.alive = false

			i.mu.Unlock()

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

// rccArgv is the command line that starts an RCC instance on a port
func rccArgv(port int) []string {
	argv := []string{exePath, "-Console", strconv.Itoa(port)}

	if runtime.GOOS != "windows" {
		argv = append([]string{"wine"}, argv...)
	}

	return argv
}

// trackPid finds the pid of a detached instance, so it can be killed later
func (i *rccInstance) trackPid() {
	for range 15 {
		time.Sleep(1 * time.Second)

		if pid, err := findPid(i.port); err != nil {
			Log(c.InYellow(fmt.Sprintf("Pid lookup failed for instance %d: %v", i.port, err)))
		} else if pid > 0 {
			i.mu.Lock()

			i.pid = pid

			i.mu.Unlock()

			return
		}
	}
}

// findPid looks up the pid of the RCCService process running a -Console <port>
func findPid(port int) (int, error) {
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", `(Get-CimInstance Win32_Process -Filter "Name='RCCService.exe'" -ErrorAction Stop | Where-Object { $_.CommandLine -match [regex]::Escape('-Console `+strconv.Itoa(port)+`') }).ProcessId`).Output()
	if err != nil {
		return 0, err
	}

	return strconv.Atoi(strings.TrimSpace(string(out)))
}

// spawnAlive checks whether an instance's process still exists, going by the tracked pid when
// available, or by its `-Console <port>` command line otherwise
func spawnAlive(i *rccInstance) bool {
	i.mu.Lock()

	pid := i.pid

	i.mu.Unlock()

	if pid > 0 {
		out, err := exec.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/NH").Output()
		if err != nil {
			Log(c.InYellow(fmt.Sprintf("Liveness check failed for instance %d: %v", i.port, err)))

			return true // unknown: don't kill a possibly-alive instance on a fluke
		}

		return strings.Contains(string(out), "RCCService.exe")
	}

	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", `if (Get-CimInstance Win32_Process -Filter "Name='RCCService.exe'" -ErrorAction Stop | Where-Object { $_.CommandLine -match [regex]::Escape('-Console `+strconv.Itoa(i.port)+`') }) { Write-Output alive }`).Output()
	if err != nil {
		// if the check can't run, be conservative and let the instance live
		Log(c.InYellow(fmt.Sprintf("Liveness check failed for instance %d: %v", i.port, err)))

		return true
	}

	return strings.TrimSpace(string(out)) == "alive"
}

// spawnDetached launches an RCCService instance exactly like a manual start: a fully detached,
// separate console process. Liveness is polled instead of waiting on process handles
func spawnDetached(i *rccInstance) error {
	absExe, err := filepath.Abs(exePath)
	if err != nil {
		return err
	}

	cmd := exec.Command("cmd", "/C", "start", "RCCService "+strconv.Itoa(i.port), absExe, "-Console", strconv.Itoa(i.port))
	cmd.Dir = filepath.Dir(absExe)

	// returns once cmd.exe has launched RCC; RCC keeps running on its own
	return cmd.Run()
}

// waitForRCC polls an instance's SOAP port until it responds
func waitForRCC(port int) error {
	for start := time.Now(); time.Since(start) < rccStartTimeout; time.Sleep(100 * time.Millisecond) {
		if res, err := http.Get(fmt.Sprintf("http://localhost:%d", port)); err == nil {
			res.Body.Close()

			return nil
		}
	}

	return fmt.Errorf("RCC instance on port %d did not respond within %s", port, rccStartTimeout)
}

// getJob returns the job with an id, or nil
func (p *pool) getJob(id string) *job {
	p.mu.Lock()

	defer p.mu.Unlock()

	return p.jobs[id]
}

// removeJob forgets a job, unless its id has since been taken by another one
func (p *pool) removeJob(j *job) {
	p.mu.Lock()

	defer p.mu.Unlock()

	if p.jobs[j.id] == j {
		delete(p.jobs, j.id)
	}
}

// instanceOf reads a job's instance under the pool lock
func (p *pool) instanceOf(j *job) *rccInstance {
	p.mu.Lock()

	defer p.mu.Unlock()

	return j.instance
}

// stopRenewing ends a job's lease renewal. Crash cleanup, closing and failed submits can race to do this, and closing the channel twice would panic
func (j *job) stopRenewing() {
	j.stopOnce.Do(func() {
		close(j.stopRenew)
	})
}

// countJobs returns how many jobs an instance holds
func (i *rccInstance) countJobs() int {
	i.mu.Lock()

	defer i.mu.Unlock()

	return len(i.jobs)
}

// kill stops an instance: alive=false, then removes the process itself
func (i *rccInstance) kill() {
	i.mu.Lock()

	i.alive = false
	child := i.child
	pid := i.pid

	i.mu.Unlock()

	switch {
	case child != nil && child.Process != nil:
		child.Process.Kill()
	case pid > 0:
		if err := exec.Command("taskkill", "/F", "/PID", strconv.Itoa(pid), "/T").Run(); err != nil {
			Log(c.InRed(fmt.Sprintf("Failed to kill RCCService instance %d (pid %d): %v", i.port, pid, err)))
		}
	}

	Log(c.InYellow(fmt.Sprintf("RCCService instance on port %d shut down", i.port)))
}

// idleShutdown closes an instance if it's a host instance with no active jobs left;
// the always-on render instance is never shut down this way
func (p *pool) idleShutdown(instance *rccInstance) {
	if instance == nil || instance.kind != "host" {
		return
	}

	if instance.countJobs() > 0 {
		return
	}

	Log(c.InYellow(fmt.Sprintf("Host instance on port %d has no active jobs, closing it...", instance.port)))
	instance.kill()
}

// freeRenderSlot frees a render job's slot once the Site has been told about it.
// Host jobs are closed with CloseJob
func (p *pool) freeRenderSlot(id string) {
	j := p.getJob(id)
	if j == nil || j.kind != "render" {
		return
	}

	if instance := p.instanceOf(j); instance != nil {
		instance.mu.Lock()

		delete(instance.jobs, id)

		instance.mu.Unlock()
	}

	p.removeJob(j)
}

// CloseJob sends a CloseJob request to the job's instance and frees its slot. Closing an unknown job does nothing
func (p *pool) CloseJob(id string) {
	j := p.getJob(id)
	if j == nil {
		return
	}

	j.closeOnce.Do(func() {
		j.stopRenewing()

		instance := p.instanceOf(j)
		if instance != nil {
			// this RCC build only implements CloseJob (CloseJobEx doesn't exist on it)
			const method = "CloseJob"

			body := strings.ReplaceAll(strings.ReplaceAll(closeTemplate, "_TASK_ID", xmlText.Replace(id)), "CloseJobEx", method)

			if req, err := http.NewRequest("POST", fmt.Sprintf("http://localhost:%d", instance.port), strings.NewReader(body)); err != nil {
				Log(c.InRed(fmt.Sprintf("Failed to create close request for job %s: %s", id, err.Error())))
			} else {
				req.Header.Set("Content-Type", "text/xml; charset=utf-8")
				req.Header.Set("SOAPAction", "http://roblox.com/"+method)

				if res, err := p.client.Do(req); err != nil {
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

		p.removeJob(j)
		p.idleShutdown(instance)
	})
}

// renewLoop keeps a hosting job's lease renewed while the pool holds it
func (p *pool) renewLoop(j *job) {
	interval := time.Duration(p.renewSeconds) * time.Second / renewAt
	ticker := time.NewTicker(interval)

	defer ticker.Stop()

	for {
		select {
		case <-j.stopRenew:
			return
		case <-ticker.C:
			instance := p.instanceOf(j)
			if instance == nil {
				return
			}

			instance.mu.Lock()

			alive := instance.alive
			_, stillOnInstance := instance.jobs[j.id]

			instance.mu.Unlock()

			if !alive || !stillOnInstance {
				return
			}

			body := strings.ReplaceAll(renewTemplate, "_TASK_ID", xmlText.Replace(j.id))
			body = strings.ReplaceAll(body, "_RENEW_SECONDS", strconv.Itoa(p.renewSeconds))

			req, err := http.NewRequest("POST", fmt.Sprintf("http://localhost:%d", instance.port), strings.NewReader(body))
			if err != nil {
				Log(c.InYellow(fmt.Sprintf("Failed to create lease renewal request for job %s: %s", j.id, err.Error())))

				continue
			}

			req.Header.Set("Content-Type", "text/xml; charset=utf-8")
			req.Header.Set("SOAPAction", "http://roblox.com/RenewLease")

			res, err := p.client.Do(req)
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
			// log it instead of sending more renewals for nothing
			if strings.Contains(string(response), "SOAP-ENV:Fault") {
				Log(c.InRed(fmt.Sprintf("RCCService rejected lease renewal of job %s: %s; stopping renewals", j.id, string(response))))

				return
			}
		}
	}
}

// Submit adds a job to the pool, sends the SOAP request to an instance (spawning a new instance
// if they're all full), and starts lease renewal for host jobs
func (p *pool) Submit(id, kind, soap string) error {
	p.mu.Lock()

	if _, exists := p.jobs[id]; exists {
		p.mu.Unlock()

		return errors.New("job " + id + " already exists")
	}

	j := &job{
		id: id,
		kind: kind,
		stopRenew: make(chan struct{}),
	}

	p.jobs[id] = j

	p.mu.Unlock()

	var instance *rccInstance

	// fail undoes everything this submit has taken so far
	fail := func(err error) error {
		if instance != nil {
			instance.mu.Lock()

			delete(instance.jobs, id)

			instance.mu.Unlock()
		}

		j.stopRenewing()
		p.removeJob(j)

		return err
	}

	instance, ok := p.pickInstance(kind)
	if !ok {
		Log(c.InBlue(fmt.Sprintf("All %s RCCService instances are full, spawning a new one...", kind)))

		var err error

		instance, err = p.spawn(kind)
		if err != nil {
			return fail(fmt.Errorf("spawn new RCCService instance: %w", err))
		}
	}

	// a close request for the same ID may have removed the job while an instance was found
	p.mu.Lock()

	if p.jobs[id] != j {
		p.mu.Unlock()

		return fail(errors.New("job " + id + " was closed before it started"))
	}

	j.instance = instance

	p.mu.Unlock()

	instance.mu.Lock()

	instance.jobs[id] = j
	started := instance.started

	instance.mu.Unlock()

	if !started {
		if err := waitForRCC(instance.port); err != nil {
			return fail(err)
		}

		instance.mu.Lock()

		instance.started = true

		instance.mu.Unlock()
	}

	req, err := http.NewRequest("POST", fmt.Sprintf("http://localhost:%d", instance.port), strings.NewReader(soap))
	if err != nil {
		return fail(err)
	}

	req.Header.Set("Content-Type", "text/xml; charset=utf-8")
	req.Header.Set("SOAPAction", "http://roblox.com/OpenJobEx")

	res, err := p.client.Do(req)
	if err != nil {
		return fail(fmt.Errorf("send request to RCCService: %w", err))
	}

	response, err := io.ReadAll(res.Body)

	res.Body.Close()

	if err != nil {
		return fail(fmt.Errorf("read RCCService response: %w", err))
	}

	// RCC returns SOAP faults for malformed requests instead of proper HTTP errors;
	// don't count these jobs as active
	if strings.Contains(string(response), "SOAP-ENV:Fault") {
		return fail(fmt.Errorf("RCCService returned a SOAP fault: %s", string(response)))
	}

	Log(c.InBlue(fmt.Sprintf("[submit] %s (%s) on instance port %d - RCC replied: %s", id, kind, instance.port, strings.TrimSpace(string(response)))))
	Log(c.InGreen(fmt.Sprintf("Job %s (%s) started on instance port %d", id, kind, instance.port)))

	// hosting jobs get their leases renewed, render jobs expire by themselves (30s)
	if kind == "host" && p.renew {
		go p.renewLoop(j)
	}

	return nil
}
