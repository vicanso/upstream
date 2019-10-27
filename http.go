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
	// UpstreamUnknown upstream unknown
	UpstreamUnknown int32 = iota
	// UpstreamSick upstream sick
	UpstreamSick
	// UpstreamHealthy upstream healthy
	UpstreamHealthy
	// UpstreamIgnored upstream ignored
	UpstreamIgnored

	// UserAgent user agent for http check
	UserAgent = "upstream/go"

	maxUint32 = ^uint32(0)
)

const (
	// PolicyFirst get the first avaliable upstream
	PolicyFirst = "first"
	// PolicyRandom get random avaliable upstream
	PolicyRandom = "random"
	// PolicyRoundRobin get avaliable upstream round robin
	PolicyRoundRobin = "roundRobin"
	// PolicyLeastconn get least connection upstream
	PolicyLeastconn = "leastconn"
)

const (
	defaultTimeout             = time.Second * 3
	defaultCheckInterval       = time.Second * 5
	healthChecking       int32 = 1
	healthCheckStop      int32 = 0

	defaultHealthCheckCount = 5
	defaultMaxFailCount     = 2
)

type (
	// Done done function for upstream
	// it should be call when upstream is used done
	Done func()
	// HTTPUpstream http upstream
	HTTPUpstream struct {
		// URL upstream url
		URL *url.URL
		// Backup backup upstream
		Backup bool
		// status upstream status(1 healthy)
		status int32
		// value upstream value(such as connection)
		value uint32
	}
	// HTTP http upstream
	HTTP struct {
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
		// Policy upstream choose policy
		Policy string

		// healthCheckStatus health check status(1 checking)
		healthCheckStatus int32
		upstreamList      []*HTTPUpstream
		// roundRobin round robin count
		roundRobin uint32
		// statusListeners status listener list
		statusListeners []StatusListener
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

// ConvertStatusToString convert status to string
func ConvertStatusToString(status int32) string {
	switch status {
	case UpstreamUnknown:
		return "unknown"
	case UpstreamSick:
		return "sick"
	case UpstreamHealthy:
		return "healthy"
	default:
		return "ignored"
	}
}

// Inc increase value of upstream
func (hu *HTTPUpstream) Inc() {
	atomic.AddUint32(&hu.value, 1)
}

// Dec decrease value o upstream
func (hu *HTTPUpstream) Dec() {
	atomic.AddUint32(&hu.value, ^uint32(0))
}

// Healthy set the http upstream to be healthy
func (hu *HTTPUpstream) Healthy() {
	atomic.StoreInt32(&hu.status, UpstreamHealthy)
}

// Sick set the http upstream to be sick
func (hu *HTTPUpstream) Sick() {
	atomic.StoreInt32(&hu.status, UpstreamSick)
}

// Ignored set the http upstream to be ignored
func (hu *HTTPUpstream) Ignored() {
	atomic.StoreInt32(&hu.status, UpstreamIgnored)
}

// Status get upstream status
func (hu *HTTPUpstream) Status() int32 {
	return atomic.LoadInt32(&hu.status)
}

// StatusDesc get upstream status description
func (hu *HTTPUpstream) StatusDesc() string {
	v := atomic.LoadInt32(&hu.status)
	return ConvertStatusToString(v)
}

func (h *HTTP) addUpstream(upstream string, backup bool) (err error) {
	info, err := url.Parse(upstream)
	if err != nil {
		return
	}
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
	if timeout == 0 {
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
	req.Header.Set("User-Agent", UserAgent)
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

		// 如果当前upstream 设置为ignored，则忽略
		if currentStatus == UpstreamIgnored {
			return
		}

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
	if interval == 0 {
		interval = defaultCheckInterval
	}
	ticker := time.NewTicker(interval)

	v := atomic.SwapInt32(&h.healthCheckStatus, healthChecking)
	// 如果已经是启动状态，则退出
	if v == healthChecking {
		return
	}
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

// PolicyFirst get the first backend
func (h *HTTP) PolicyFirst() *HTTPUpstream {
	return h.GetAvailableUpstream(0)
}

// PolicyRandom get random backend
func (h *HTTP) PolicyRandom() *HTTPUpstream {
	return h.GetAvailableUpstream(rand.Uint32())
}

// PolicyRoundRobin get backend round robin
func (h *HTTP) PolicyRoundRobin() *HTTPUpstream {
	index := atomic.AddUint32(&h.roundRobin, 1)
	return h.GetAvailableUpstream(index)
}

// PolicyLeastconn get least connection backend
func (h *HTTP) PolicyLeastconn() *HTTPUpstream {
	preferredList, backupList := h.getDivideAvailableUpstreamList()

	upstreamList := preferredList
	if len(upstreamList) == 0 {
		upstreamList = backupList
	}
	upstreamCount := uint32(len(upstreamList))

	// 如果无可用backend
	if upstreamCount == 0 {
		return nil
	}

	leastConn := maxUint32
	index := 0
	// 查找连接数最少的 upstream
	for i, upstream := range upstreamList {
		v := atomic.LoadUint32(&upstream.value)
		if v < leastConn {
			leastConn = v
			index = i
		}
	}

	return upstreamList[uint32(index)%upstreamCount]
}

// GetAvailableUpstream get available upstream
func (h *HTTP) GetAvailableUpstream(index uint32) (upstream *HTTPUpstream) {
	preferredList, backupList := h.getDivideAvailableUpstreamList()

	upstreamList := preferredList
	if len(upstreamList) == 0 {
		upstreamList = backupList
	}

	upstreamCount := uint32(len(upstreamList))

	// 如果无可用backend
	if upstreamCount == 0 {
		return
	}

	upstream = upstreamList[index%upstreamCount]

	return
}

// Next get the next available upstream by policy
func (h *HTTP) Next() (upstream *HTTPUpstream, done Done) {
	switch h.Policy {
	case PolicyFirst:
		upstream = h.PolicyFirst()
	case PolicyRandom:
		upstream = h.PolicyRandom()
	case PolicyLeastconn:
		upstream = h.PolicyLeastconn()
		if upstream != nil {
			done = func() {
				atomic.AddUint32(&upstream.value, ^uint32(0))
			}
		}

	default:
		upstream = h.PolicyRoundRobin()
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
