package upstream

import (
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// UpstreamSick upstream sick
	UpstreamSick int32 = iota
	// UpstreamHealthy upstream healthy
	UpstreamHealthy
)

const (
	defaultTimeout             = time.Second * 3
	defaultCheckInterval       = time.Second * 5
	healthChecking       int32 = 1
	healthCheckStop      int32 = 0

	defaultHealthCheckCount = 5
	defaultMaxFailCount     = 2

	// PolicyFirst first avalid upstream
	PolicyFirst = "first"
	// PolicyRandom random avalid upstream
	PolicyRandom = "random"
	// PolicyRoundRobin round robin upstream
	PolicyRoundRobin = "roundRobin"
)

type (
	// HTTPUpstream http upstream
	HTTPUpstream struct {
		// URL upstream url
		URL *url.URL
		// Backup backup upstream
		Backup bool
		// Status upstream status(1 healthy)
		status int32
	}
	// HTTP http upstream
	HTTP struct {
		// Policy policy
		Policy string
		// Ping http ping url
		Ping string
		// Timeout timeout for health check
		Timeout time.Duration
		// Interval health check interval
		Interval time.Duration
		// HealthCheckCount health check count
		HealthCheckCount int
		// MaxFailCount max fail count, if fail count >=, it will be set to sick
		MaxFailCount int

		// healthCheckStatus health check status(1 checking)
		healthCheckStatus int32
		upstreamList      []*HTTPUpstream
		// roundRobin round robin count
		roundRobin uint32
		// statusListeners status listener list
		statusListeners []StatusListener

		sync.RWMutex
	}
	// StatusListener upstream status listener
	StatusListener func(status int32, upstream *HTTPUpstream)
)

// portCheck the port check
func portCheck(network, ip, port string, timeout time.Duration) (healthy bool, err error) {
	addr := ip
	if port != "" {
		addr = net.JoinHostPort(ip, port)
	}
	conn, err := net.DialTimeout(network, addr, timeout)
	if err != nil {
		return
	}
	defer conn.Close()
	healthy = true
	return
}

func (h *HTTP) addUpstream(upstream string, backup bool) (err error) {
	info, err := url.Parse(upstream)
	if err != nil {
		return
	}
	h.Lock()
	defer h.Unlock()
	if h.upstreamList == nil {
		h.upstreamList = make([]*HTTPUpstream, 0)
	}
	h.upstreamList = append(h.upstreamList, &HTTPUpstream{
		URL:    info,
		Backup: backup,
	})
	return
}

// Add add upstream
func (h *HTTP) Add(upstream string) (err error) {
	return h.addUpstream(upstream, false)
}

// AddBackup add backup stream
func (h *HTTP) AddBackup(upstream string) (err error) {
	return h.addUpstream(upstream, true)
}

// ping health check ping
func (h *HTTP) ping(info *url.URL) (healthy bool, err error) {
	timeout := h.Timeout
	if timeout.Nanoseconds() == 0 {
		timeout = defaultTimeout
	}
	// 如果没有配置ping，则检测端口
	if h.Ping == "" {
		port := info.Port()
		if port == "" {
			port = "80"
		}
		return portCheck("tcp", info.Hostname(), port, timeout)
	}
	// 如果配置了ping，则将用http 请求，判断返回码
	client := &http.Client{
		Timeout: timeout,
	}
	pingURL := info.String() + h.Ping
	req, err := http.NewRequest(http.MethodGet, pingURL, nil)
	if err != nil {
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	statusCode := resp.StatusCode
	if statusCode >= http.StatusOK && statusCode < http.StatusBadRequest {
		healthy = true
	}
	return
}

// DoHealthCheck do health check
func (h *HTTP) DoHealthCheck() {
	healthCheckCount := h.HealthCheckCount
	if healthCheckCount <= 0 {
		healthCheckCount = defaultHealthCheckCount
	}
	maxFailCount := h.MaxFailCount
	if maxFailCount <= 0 {
		maxFailCount = defaultMaxFailCount
	}

	// 对upstream 进行状态检测
	doCheck := func(upstream *HTTPUpstream) {
		var wg sync.WaitGroup
		var failCount int32
		for i := 0; i < healthCheckCount; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				healthy, _ := h.ping(upstream.URL)
				// 如果失败，则加1
				if !healthy {
					atomic.AddInt32(&failCount, 1)
				}
			}()
		}
		wg.Wait()

		pStatus := &upstream.status
		currentStatus := atomic.LoadInt32(pStatus)
		status := UpstreamHealthy
		if int(failCount) >= maxFailCount {
			status = UpstreamSick
		}
		// 如果状态有变更
		if currentStatus != status {
			atomic.StoreInt32(pStatus, status)
			// 触发状态变更时的回调
			for _, fn := range h.statusListeners {
				fn(status, upstream)
			}
		}
	}
	// 立即执行一次health check
	for _, upstream := range h.upstreamList {
		doCheck(upstream)
	}
}

// StartHealthCheck start health check
func (h *HTTP) StartHealthCheck() {
	interval := h.Interval
	if interval.Nanoseconds() == 0 {
		interval = defaultCheckInterval
	}
	ticker := time.NewTicker(interval)

	atomic.StoreInt32(&h.healthCheckStatus, healthChecking)
	for range ticker.C {
		v := atomic.LoadInt32(&h.healthCheckStatus)
		// 如果已停止health check，则退出
		if v != healthChecking {
			return
		}
		h.DoHealthCheck()
	}
}

// StopHealthCheck 停止health check
func (h *HTTP) StopHealthCheck() {
	atomic.StoreInt32(&h.healthCheckStatus, healthCheckStop)
}

func (h *HTTP) getDivideAvailableUpstreamList() (preferredList []*HTTPUpstream, backupList []*HTTPUpstream) {
	// 优先使用的 upstream
	preferredList = make([]*HTTPUpstream, 0, len(h.upstreamList))
	// 备份使用的 upstream
	backupList = make([]*HTTPUpstream, 0, len(h.upstreamList)/2)
	h.RLock()
	defer h.RUnlock()
	// 从当前 upstream 列表中筛选
	for _, upstream := range h.upstreamList {
		status := atomic.LoadInt32(&upstream.status)
		if status == UpstreamHealthy {
			if upstream.Backup {
				backupList = append(backupList, upstream)
			} else {
				preferredList = append(preferredList, upstream)
			}
		}
	}
	return
}

// GetAvailableUpstreamList get available upstream list
func (h *HTTP) GetAvailableUpstreamList() (upstreamList []*HTTPUpstream) {
	upstreamList = make([]*HTTPUpstream, 0, len(h.upstreamList))
	preferredList, backupList := h.getDivideAvailableUpstreamList()
	// 合并返回
	upstreamList = append(upstreamList, preferredList...)
	upstreamList = append(upstreamList, backupList...)
	return
}

// GetAvailableUpstream get available upstream
func (h *HTTP) GetAvailableUpstream(index uint32) (upstream *HTTPUpstream) {
	preferredList, backupList := h.getDivideAvailableUpstreamList()

	preferredCount := uint32(len(preferredList))
	backupCount := uint32(len(backupList))

	// 如果无可用backend
	if preferredCount+backupCount == 0 {
		return
	}

	switch h.Policy {
	case PolicyFirst:
		index = 0
	case PolicyRandom:
		index = rand.Uint32()
	case PolicyRoundRobin:
		index = atomic.AddUint32(&h.roundRobin, 1)
	default:
		// 默认的使用参数index
		break
	}

	if preferredCount != 0 {
		upstream = preferredList[index%preferredCount]
	} else if backupCount != 0 {
		upstream = backupList[index%backupCount]
	}
	return
}

// OnStatus add listener for status event
func (h *HTTP) OnStatus(listener StatusListener) {
	if h.statusListeners == nil {
		h.statusListeners = make([]StatusListener, 0)
	}
	h.statusListeners = append(h.statusListeners, listener)
}

// GetUpstreamList get all upstream
func (h *HTTP) GetUpstreamList() []*HTTPUpstream {
	return h.upstreamList
}
