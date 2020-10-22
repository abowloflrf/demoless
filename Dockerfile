FROM golang:1.15 as builder
WORKDIR /code
COPY go.mod go.sum /code/
RUN go version && go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o demoless

FROM alpine:3.12
COPY --from=builder /code/demoless /usr/bin/demoless
ENTRYPOINT [ "/usr/bin/demoless" ]
CMD [ "--help" ]