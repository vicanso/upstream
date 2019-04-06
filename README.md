# upstream

[![Build Status](https://img.shields.io/travis/vicanso/upstream.svg?label=linux+build)](https://travis-ci.org/vicanso/upstream)


It's easy to get upstream from upstream list. It support http request check or tcp port check.


```go
uh := upstream.HTTP{
  // use http request check
  Ping: "/ping",
}
uh.Add("http://127.0.0.1:7001")
uh.Add("http://127.0.0.1:7002")
// do health check
uh.DoHealthCheck()
// do health check loop(default interval: 5s )
go uh.StartHealthCheck()
// get valid upstream by round robin
upstream := uh.PolicyRoundRobin()
```
