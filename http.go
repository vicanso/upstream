package upstream

import (
	"crypto/tls"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"go.uber.org/atomic"
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
)

const (
	// UserAgent user agent for http check
	UserAgent = "upstream/go"
	maxUint32 = uint32(math.MaxUint32)
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

var httpTransport = &http.Transport{
	Proxy: http.ProxyFromEnvironment,
	DialContext: (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext,
	ForceAttemptHTTP2:     true,
	MaxIdleConns:          10,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
	TLSClientConfig: &tls.Config{
		// upstream主要用于内部的检测，因此忽略tls证书
		InsecureSkipVerify: true,
	},
}

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
		status atomic.Int32
		// value upstream value(such as connection count)
		value atomic.Uint32
	}
	// HTTP http upstream
	HTTP struct {
		// Host the http host
		Host string
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
		healthCheckStatus atomic.Int32
		upstreamList      []*HTTPUpstream
		// roundRobin round robin count
		roundRobin atomic.Uint32
		// statusListeners status listener list
		statusListeners []StatusListener
	}
	// StatusListener upstream status listener
	StatusListener func(status int32, upstream *HTTPUpstream)
)

// portCheck the port check
func portCheck(network, ip, port string, timeout time.Duration) (bool, error) {
	addr := ip
	if port != "" {
		addr = net.JoinHostPort(ip, port)
	}
	conn, err := net.DialTimeout(network, addr, timeout)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	return true, nil
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
func (hu *HTTPUpstream) Inc() uint32 {
	return hu.value.Inc()
}

// Dec decrease value o upstream
func (hu *HTTPUpstream) Dec() uint32 {
	return hu.value.Dec()
}

// Healthy set the http upstream to be healthy
func (hu *HTTPUpstream) Healthy() {
	hu.status.Store(UpstreamHealthy)
}

// Sick set the http upstream to be sick
func (hu *HTTPUpstream) Sick() {
	hu.status.Store(UpstreamSick)
}

// Ignored set the http upstream to be ignored
func (hu *HTTPUpstream) Ignored() {
	hu.status.Store(UpstreamIgnored)
}

// Status get upstream status
func (hu *HTTPUpstream) Status() int32 {
	return hu.status.Load()
}

// StatusDesc get upstream status description
func (hu *HTTPUpstream) StatusDesc() string {
	v := hu.Status()
	return ConvertStatusToString(v)
}

func (h *HTTP) addUpstream(upstream string, backup bool) error {
	info, err := url.Parse(upstream)
	if err != nil {
		return err
	}
	if h.upstreamList == nil {
		h.upstreamList = make([]*HTTPUpstream, 0)
	}
	h.upstreamList = append(h.upstreamList, &HTTPUpstream{
		URL:    info,
		Backup: backup,
	})
	return nil
}

// Add add upstream
func (h *HTTP) Add(upstream string) error {
	return h.addUpstream(upstream, false)
}

// AddBackup add backup stream
func (h *HTTP) AddBackup(upstream string) error {
	return h.addUpstream(upstream, true)
}

// ping health check ping
func (h *HTTP) ping(info *url.URL) (bool, error) {
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
		Timeout:   timeout,
		Transport: httpTransport,
	}
	pingURL := info.String() + h.Ping
	req, err := http.NewRequest(http.MethodGet, pingURL, nil)
	if err != nil {
		return false, err
	}
	if h.Host != "" {
		req.Host = h.Host
	}
	req.Header.Set("User-Agent", UserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	statusCode := resp.StatusCode
	if statusCode >= http.StatusOK && statusCode < http.StatusBadRequest {
		return true, nil
	}
	return false, nil
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
		currentStatus := upstream.status.Load()

		// 如果当前upstream 设置为ignored，则忽略
		if currentStatus == UpstreamIgnored {
			return
		}
		var wg sync.WaitGroup
		failCount := atomic.NewInt32(0)
		for i := 0; i < healthCheckCount; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				// 出错也忽略，只判断是否检测成功则可
				healthy, _ := h.ping(upstream.URL)
				// 如果失败，则加1
				if !healthy {
					failCount.Inc()
				}
			}()
		}
		wg.Wait()

		status := UpstreamHealthy
		if int(failCount.Load()) >= maxFailCount {
			status = UpstreamSick
		}
		// 如果状态有变更
		if currentStatus != status {
			upstream.status.Store(status)
			// 触发状态变更时的回调
			for _, fn := range h.statusListeners {
				fn(status, upstream)
			}
		}
	}
	// 对所有upstream执行health check
	for _, upstream := range h.upstreamList {
		// ignore的不需要检测
		if upstream.status.Load() == UpstreamIgnored {
			continue
		}
		doCheck(upstream)
	}
}

// StartHealthCheck start health check
func (h *HTTP) StartHealthCheck() {
	// 建议在调用start health check定时检测前，先调用一次DoHealthCheck，
	// 之后以新的goroutine执行StartHealthCheck
	interval := h.Interval
	if interval <= 0 {
		interval = defaultCheckInterval
	}
	ticker := time.NewTicker(interval)

	v := h.healthCheckStatus.Swap(healthChecking)
	// 如果已经是启动状态，则退出
	if v == healthChecking {
		return
	}
	for range ticker.C {
		v := h.healthCheckStatus.Load()
		// 如果已停止health check，则退出
		if v != healthChecking {
			return
		}
		h.DoHealthCheck()
	}
}

// StopHealthCheck 停止health check
func (h *HTTP) StopHealthCheck() {
	h.healthCheckStatus.Store(healthCheckStop)
}

func (h *HTTP) getDivideAvailableUpstreamList() ([]*HTTPUpstream, []*HTTPUpstream) {
	// 优先使用的 upstream
	preferredList := make([]*HTTPUpstream, 0, len(h.upstreamList))
	// 备份使用的 upstream
	backupList := make([]*HTTPUpstream, 0, len(h.upstreamList)/2)
	// 从当前 upstream 列表中筛选
	for _, upstream := range h.upstreamList {
		status := upstream.status.Load()
		if status == UpstreamHealthy {
			if upstream.Backup {
				backupList = append(backupList, upstream)
			} else {
				preferredList = append(preferredList, upstream)
			}
		}
	}
	return preferredList, backupList
}

// 优先获取非backup的服务列表，如果都不可用，则使用backup
func (h *HTTP) enhanceGetAvailableUpstreamList() []*HTTPUpstream {
	preferredList, backupList := h.getDivideAvailableUpstreamList()
	if len(preferredList) != 0 {
		return preferredList
	}
	return backupList
}

// GetAvailableUpstreamList get available upstream list
func (h *HTTP) GetAvailableUpstreamList() []*HTTPUpstream {
	upstreamList := make([]*HTTPUpstream, 0, len(h.upstreamList))
	preferredList, backupList := h.getDivideAvailableUpstreamList()
	// 合并返回
	upstreamList = append(upstreamList, preferredList...)
	upstreamList = append(upstreamList, backupList...)
	return upstreamList
}

// PolicyFirst get the first backend
func (h *HTTP) PolicyFirst() *HTTPUpstream {
	return h.GetAvailableUpstream(0)
}

// PolicyRandom get random backend
func (h *HTTP) PolicyRandom() *HTTPUpstream {
	rand.Seed(time.Now().UnixNano())
	return h.GetAvailableUpstream(rand.Uint32())
}

// PolicyRoundRobin get backend round robin
func (h *HTTP) PolicyRoundRobin() *HTTPUpstream {
	index := h.roundRobin.Inc()
	return h.GetAvailableUpstream(index)
}

// PolicyLeastconn get least connection backend
func (h *HTTP) PolicyLeastconn() *HTTPUpstream {
	upstreamList := h.enhanceGetAvailableUpstreamList()
	upstreamCount := uint32(len(upstreamList))

	// 如果无可用backend
	if upstreamCount == 0 {
		return nil
	}

	leastConn := maxUint32
	index := 0
	// 查找连接数最少的 upstream
	for i, upstream := range upstreamList {
		v := upstream.value.Load()
		if v < leastConn {
			leastConn = v
			index = i
		}
	}

	return upstreamList[uint32(index)%upstreamCount]
}

// GetAvailableUpstream get available upstream
func (h *HTTP) GetAvailableUpstream(index uint32) (upstream *HTTPUpstream) {
	upstreamList := h.enhanceGetAvailableUpstreamList()
	upstreamCount := uint32(len(upstreamList))

	// 如果无可用backend
	if upstreamCount == 0 {
		return
	}

	upstream = upstreamList[index%upstreamCount]

	return
}

// Next get the next available upstream by policy
func (h *HTTP) Next(index ...uint32) (*HTTPUpstream, Done) {
	if len(index) != 0 {
		return h.GetAvailableUpstream(index[0]), nil
	}
	switch h.Policy {
	case PolicyFirst:
		return h.PolicyFirst(), nil
	case PolicyRandom:
		return h.PolicyRandom(), nil
	case PolicyLeastconn:
		upstream := h.PolicyLeastconn()
		var done Done
		if upstream != nil {
			upstream.value.Inc()
			done = func() {
				upstream.value.Dec()
			}
		}
		return upstream, done

	default:
		return h.PolicyRoundRobin(), nil
	}
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
