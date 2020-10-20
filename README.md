# A prototype of zero-scale with Go & Docker

```
# create a backend container
docker run -d --name ng -p 127.0.0.1:8888:80 --health-cmd "service nginx status" --health-interval 1s nginx
docker stop ng

# backend container is stopped
./demoless

# request with curl
curl -i -vvv -X GET -H "Host: ng.demoless.app" '127.0.0.1:8080'
```
