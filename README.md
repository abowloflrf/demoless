# A prototype of zero-scale with Go & Docker

```
# create a backend container
docker run -d --name ng -p 127.0.0.1:8888:80 --health-cmd "service nginx status" --health-interval 1s nginx
docker stop ng

# backend container is stopped
./demoless -c ng
```

Run HTTP load test with `wrk`

```
❯ wrk -c 2 -t 1 -d 10s http://127.0.0.1:8000
Running 10s test @ http://127.0.0.1:8000
  1 threads and 2 connections
  Thread Stats   Avg      Stdev     Max   +/- Stdev
    Latency   107.13ms  319.64ms   1.61s    90.26%
    Req/Sec     2.44k   431.62     2.78k    78.82%
  20700 requests in 10.01s, 16.31MB read
Requests/sec:   2067.57
Transfer/sec:      1.63MB
```

Therer is no error response
