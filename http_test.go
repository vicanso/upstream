package upstream

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestHTTPAdd(t *testing.T) {
	assert := assert.New(t)
	h := HTTP{}
	err := h.Add("127.0.0.1:3000")
	assert.NotNil(err, "invalid url should return error")
	err = h.Add("http://127.0.0.1:3000")
	assert.Nil(err, "add upstream should be successful")
	assert.Equal(1, len(h.upstreamList), "upstream count should be 1")
}

func createServe(handler http.Handler) (l net.Listener, err error) {
	l, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return
	}
	go func() {
		srv := http.Server{
			Handler: handler,
		}
		srv.Serve(l)
	}()
	return
}

func TestHTTPPing(t *testing.T) {
	t.Run("port health check", func(t *testing.T) {
		assert := assert.New(t)
		ln, err := createServe(nil)
		assert.Nil(err, "create server should be successful")
		defer ln.Close()
		h := HTTP{}
		err = h.Add("http://" + ln.Addr().String())
		assert.Nil(err, "add upstream should be successful")

		healthy, err := h.ping(h.upstreamList[0].URL)
		assert.Nil(err, "tcp ping should be successful")
		assert.True(healthy, "upstream should be healthy")
	})

	t.Run("http health check", func(t *testing.T) {
		assert := assert.New(t)
		ln, err := createServe(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.RequestURI == "/ping" {
				w.Write([]byte("pong"))
				return
			}
			w.WriteHeader(500)
			w.Write([]byte("error"))
		}))
		assert.Nil(err, "create server should be successful")

		defer ln.Close()
		h := HTTP{
			Ping: "/ping",
		}
		err = h.Add("http://" + ln.Addr().String())
		assert.Nil(err, "add upstream should be successful")

		healthy, err := h.ping(h.upstreamList[0].URL)
		assert.Nil(err, "http ping should be successful")
		assert.True(healthy, "upstream should be healthy")
	})
}

func TestHealthCheck(t *testing.T) {
	t.Run("healthy", func(t *testing.T) {
		assert := assert.New(t)
		ln, err := createServe(nil)
		assert.Nil(err, "create server should be successful")
		defer ln.Close()
		h := HTTP{
			Interval: 10 * time.Millisecond,
		}
		err = h.Add("http://" + ln.Addr().String())
		assert.Nil(err, "add upstream should be successful")
		go func() {
			time.Sleep(time.Second)
			h.StopHealthCheck()
		}()
		h.StartHealthCheck()
		arr := h.GetAvailableUpstreamList()
		assert.Equal(1, len(arr), "count avaliable upstream should be 1")
	})

	t.Run("sick", func(t *testing.T) {
		assert := assert.New(t)
		h := HTTP{
			Interval: 10 * time.Millisecond,
		}
		err := h.Add("http://127.0.0.1:12344")
		assert.Nil(err, "add upstream should be successful")
		h.GetUpstreamList()[0].status = UpstreamHealthy
		h.DoHealthCheck()
		assert.Equal(UpstreamSick, h.GetUpstreamList()[0].status, "upstream should be sick")
	})
}

func TestGetAvalidUpstream(t *testing.T) {
	assert := assert.New(t)
	h := HTTP{}
	h.Add("http://127.0.0.1:7001")
	h.Add("http://127.0.0.1:7002")
	h.Add("http://127.0.0.1:7003")
	for _, upstream := range h.upstreamList {
		upstream.status = UpstreamHealthy
	}

	assert.Equal(h.upstreamList[2], h.GetAvailableUpstream(2), "get upstream by index should be successful")

	// first
	for range h.upstreamList {
		target := h.PolicyFirst()
		assert.Equal(h.upstreamList[0], target, "first policy should always return the first upstream")
	}

	for index := range h.upstreamList {
		target := h.PolicyRoundRobin()
		i := (index + 1) % len(h.upstreamList)
		assert.Equal(h.upstreamList[i], target, "round robin policy should return upstream sequence")
	}

	// 除第一个，所有upstream 设置为sick
	for index, upstream := range h.upstreamList {
		if index == 0 {
			continue
		}
		upstream.status = UpstreamSick
	}
	for i := 0; i < 100; i++ {
		target := h.PolicyRandom()
		assert.Equal(h.upstreamList[0], target, "random policy shouldn't return sick upstream")
	}

	// backup
	h.AddBackup("http://127.0.0.1:7003")
	backupUpstream := h.upstreamList[len(h.upstreamList)-1]
	backupUpstream.status = UpstreamHealthy
	for i := 0; i < 100; i++ {
		target := h.PolicyRoundRobin()
		assert.NotEqual(backupUpstream, target, "backup upstream shouldn't be used when there is any available upstream")
	}
	for _, upstream := range h.upstreamList {
		if upstream == backupUpstream {
			continue
		}
		upstream.status = UpstreamSick
	}
	for i := 0; i < 100; i++ {
		target := h.PolicyRoundRobin()
		assert.Equal(backupUpstream, target, "backup upstream should be used when there isn't available upstream")
	}
	for i := 0; i < 100; i++ {
		target := h.PolicyLeastconn()
		assert.Equal(backupUpstream, target, "backup upstream should be used when there isn't available upstream")
	}

	for _, upstream := range h.upstreamList {
		upstream.status = UpstreamHealthy
		upstream.value = 1
	}
	// 将第二个upstream 的value设置为0
	secondUpstream := h.upstreamList[1]
	secondUpstream.value = 0
	for i := 0; i < 100; i++ {
		target := h.PolicyLeastconn()
		assert.Equal(secondUpstream, target, "least conn policy should return least conn's upstream")
	}
	// 第二个upstream 连接数增加
	secondUpstream.Inc()
	// 所有的连接数一致，则选择第一个
	firstUpstream := h.upstreamList[0]
	for i := 0; i < 100; i++ {
		target := h.PolicyLeastconn()
		assert.Equal(firstUpstream, target, "least conn policy should return least conn's upstream")
	}
	// 第二个upstream 连接数减少
	secondUpstream.Dec()
	for i := 0; i < 100; i++ {
		target := h.PolicyLeastconn()
		assert.Equal(secondUpstream, target, "least conn policy should return least conn's upstream")
	}

	h.Policy = PolicyLeastconn
	upstream, done := h.Next()
	assert.NotNil(upstream)
	assert.NotNil(done)

	h.Policy = PolicyFirst
	upstream, done = h.Next()
	assert.NotNil(upstream)
	assert.Nil(done)

	h.Policy = PolicyRandom
	upstream, done = h.Next()
	assert.NotNil(upstream)
	assert.Nil(done)

	h.Policy = ""
	upstream, done = h.Next()
	assert.NotNil(upstream)
	assert.Nil(done)
}

func TestHTTPUpstreamStatusChange(t *testing.T) {
	assert := assert.New(t)
	hu := &HTTPUpstream{}
	assert.Equal(UpstreamUnknown, hu.Status(), "status should be unknown when init")
	assert.Equal("unknown", hu.StatusDesc(), "status should be unknown when init")

	hu.Healthy()
	assert.Equal(UpstreamHealthy, hu.Status())

	hu.Sick()
	assert.Equal(UpstreamSick, hu.Status())

	hu.Ignored()
	assert.Equal(UpstreamIgnored, hu.Status())
}

func TestOnStatus(t *testing.T) {
	assert := assert.New(t)
	ln, err := createServe(nil)
	assert.Nil(err, "create server should be successful")

	defer ln.Close()
	h := HTTP{
		Interval: 10 * time.Millisecond,
	}
	done := false
	h.OnStatus(func(status int32, upstream *HTTPUpstream) {
		done = true
		assert.Equal(UpstreamHealthy, status)
		assert.NotNil(upstream)
	})
	err = h.Add("http://" + ln.Addr().String())
	assert.Nil(err, "add upstream should be successful")
	h.DoHealthCheck()
	assert.True(done, "on status should be called")
}

func TestConvertStatusToString(t *testing.T) {
	assert := assert.New(t)
	assert.Equal("sick", ConvertStatusToString(UpstreamSick))
	assert.Equal("healthy", ConvertStatusToString(UpstreamHealthy))
	assert.Equal("ignored", ConvertStatusToString(UpstreamIgnored))
	assert.Equal("unknown", ConvertStatusToString(UpstreamUnknown))
}

func BenchmarkRoundRobin(b *testing.B) {
	h := HTTP{}
	h.Add("http://127.0.0.1:7001")
	h.Add("http://127.0.0.1:7002")
	h.Add("http://127.0.0.1:7003")
	for _, upstream := range h.upstreamList {
		upstream.status = UpstreamHealthy
	}

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		h.PolicyRoundRobin()
	}
}

func BenchmarkLeastConn(b *testing.B) {
	h := HTTP{}
	h.Add("http://127.0.0.1:7001")
	h.Add("http://127.0.0.1:7002")
	h.Add("http://127.0.0.1:7003")
	for _, upstream := range h.upstreamList {
		upstream.status = UpstreamHealthy
	}

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		h.PolicyLeastconn()
	}
}

// https://stackoverflow.com/questions/50120427/fail-unit-tests-if-coverage-is-below-certain-percentage
func TestMain(m *testing.M) {
	// call flag.Parse() here if TestMain uses flags
	rc := m.Run()

	// rc 0 means we've passed,
	// and CoverMode will be non empty if run with -cover
	if rc == 0 && testing.CoverMode() != "" {
		c := testing.Coverage()
		if c < 0.9 {
			fmt.Println("Tests passed but coverage failed at", c)
			rc = -1
		}
	}
	os.Exit(rc)
}
