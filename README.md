# upstream

[![Build Status](https://github.com/vicanso/upstream/workflows/Test/badge.svg)](https://github.com/upstream/elton/actions)


It's easy to get upstream from upstream list. It support http request check or tcp port check.


```go
uh := upstream.HTTP{
  // use http request check
  Ping: "/ping",
}
uh.OnStatus(func(status int32, upstream *upstream.HTTPUpstream) {
})
uh.Add("http://127.0.0.1:7001")
uh.AddBackup("http://127.0.0.1:7002")
// do health check
uh.DoHealthCheck()
// do health check loop(default interval: 5s )
go uh.StartHealthCheck()
// get valid upstream by round robin
upstream := uh.PolicyRoundRobin()
```
