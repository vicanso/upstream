package upstream

import (
	"net"
	"net/http"
	"testing"
	"time"
)

func TestHTTPAdd(t *testing.T) {
	h := HTTP{}
	err := h.Add("127.0.0.1:3000")
	if err == nil {
		t.Fatalf("invalid url should return error")
	}
	err = h.Add("http://127.0.0.1:3000")
	if err != nil || len(h.upstreamList) != 1 {
		t.Fatalf("add http upstream fail, %v", err)
	}
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
		ln, err := createServe(nil)
		if err != nil {
			t.Fatalf("create server fail, %v", err)
		}
		defer ln.Close()
		h := HTTP{}
		err = h.Add("http://" + ln.Addr().String())
		if err != nil {
			t.Fatalf("add http upstream fail, %v", err)
		}
		healthy, err := h.ping(h.upstreamList[0].URL)
		if err != nil || !healthy {
			t.Fatalf("ping fail, %v", err)
		}
	})

	t.Run("http health check", func(t *testing.T) {
		ln, err := createServe(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.RequestURI == "/ping" {
				w.Write([]byte("pong"))
				return
			}
			w.WriteHeader(500)
			w.Write([]byte("error"))
		}))

		if err != nil {
			t.Fatalf("create server fail, %v", err)
		}
		defer ln.Close()
		h := HTTP{
			Ping: "/ping",
		}
		err = h.Add("http://" + ln.Addr().String())
		if err != nil {
			t.Fatalf("add http upstream fail, %v", err)
		}
		healthy, err := h.ping(h.upstreamList[0].URL)
		if err != nil || !healthy {
			t.Fatalf("ping fail, %v", err)
		}
	})
}

func TestHealthCheck(t *testing.T) {
	t.Run("healthy", func(t *testing.T) {
		ln, err := createServe(nil)
		if err != nil {
			t.Fatalf("create server fail, %v", err)
		}
		defer ln.Close()
		h := HTTP{
			Interval: 10 * time.Millisecond,
		}
		err = h.Add("http://" + ln.Addr().String())
		if err != nil {
			t.Fatalf("add http upstream fail, %v", err)
		}
		go func() {
			time.Sleep(time.Second)
			h.StopHealthCheck()
		}()
		h.StartHealthCheck()
		arr := h.GetAvailableUpstreamList()
		if len(arr) != 1 {
			t.Fatalf("health check fail")
		}
	})

	t.Run("sick", func(t *testing.T) {
		h := HTTP{
			Interval: 10 * time.Millisecond,
		}
		err := h.Add("http://127.0.0.1:12344")
		if err != nil {
			t.Fatalf("add http upstream fail, %v", err)
		}
		h.GetUpstreamList()[0].status = UpstreamHealthy
		h.DoHealthCheck()
		if h.GetUpstreamList()[0].status != UpstreamSick {
			t.Fatalf("upstream should be sick")
		}
	})
}

func TestGetAvalidUpstream(t *testing.T) {
	h := HTTP{}
	h.Add("http://127.0.0.1:7001")
	h.Add("http://127.0.0.1:7002")
	h.Add("http://127.0.0.1:7003")
	for _, upstream := range h.upstreamList {
		upstream.status = UpstreamHealthy
	}

	if h.GetAvailableUpstream(2) != h.upstreamList[2] {
		t.Fatalf("get stream by index fail")
	}

	// first
	h.Policy = PolicyFirst
	for range h.upstreamList {
		target := h.GetAvailableUpstream(1)
		if target != h.upstreamList[0] {
			t.Fatalf("get first upstream fail")
		}
	}

	h.Policy = PolicyRoundRobin
	for index := range h.upstreamList {
		target := h.GetAvailableUpstream(1)
		i := (index + 1) % len(h.upstreamList)
		if target != h.upstreamList[i] {
			t.Fatalf("get round robin upstream fail")
		}
	}

	h.Policy = PolicyRandom
	// 除第一个，所有upstream 设置为sick
	for index, upstream := range h.upstreamList {
		if index == 0 {
			continue
		}
		upstream.status = UpstreamSick
	}
	for i := 0; i < 100; i++ {
		target := h.GetAvailableUpstream(1)
		if target != h.upstreamList[0] {
			t.Fatalf("get random upstream fail")
		}
	}

	h.Policy = PolicyRoundRobin
	h.AddBackup("http://127.0.0.1:7003")
	lastUpstream := h.upstreamList[len(h.upstreamList)-1]
	lastUpstream.status = UpstreamHealthy
	for i := 0; i < 100; i++ {
		target := h.GetAvailableUpstream(1)
		if target == lastUpstream {
			t.Fatalf("backup stream should not be used")
		}
	}
}

func TestOnStatus(t *testing.T) {
	ln, err := createServe(nil)
	if err != nil {
		t.Fatalf("create server fail, %v", err)
	}
	defer ln.Close()
	h := HTTP{
		Interval: 10 * time.Millisecond,
	}
	done := false
	h.OnStatus(func(status int32, upstream *HTTPUpstream) {
		done = true
		if status != UpstreamHealthy || upstream == nil {
			t.Fatalf("on status value is invalid")
		}
	})
	err = h.Add("http://" + ln.Addr().String())
	if err != nil {
		t.Fatalf("add http upstream fail, %v", err)
	}
	h.DoHealthCheck()
	if !done {
		t.Fatalf("on status is not called")
	}
}

func BenchmarkGetAvalidUpstreamList(b *testing.B) {
	h := HTTP{
		Policy: PolicyRoundRobin,
	}
	h.Add("http://127.0.0.1:7001")
	h.Add("http://127.0.0.1:7002")
	h.Add("http://127.0.0.1:7003")
	for _, upstream := range h.upstreamList {
		upstream.status = UpstreamHealthy
	}

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		h.GetAvailableUpstreamList()
	}
}