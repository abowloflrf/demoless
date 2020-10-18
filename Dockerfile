FROM golang:1.15 as builder
WORKDIR /code
COPY . .
RUN CGO_ENABLED=0 go build -o demoless

FROM alpine:3.12
COPY --from=builder /code/demoless /usr/bin/demoless
ENTRYPOINT [ "/usr/bin/demoless" ]
CMD [ "--help" ]