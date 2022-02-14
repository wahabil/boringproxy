FROM golang:1.17-alpine3.15 as builder
LABEL boringproxy=builder

ARG GOOS="linux"
ARG GOARCH="amd64"

WORKDIR /build

RUN apk add git

COPY go.* ./
RUN go mod download
COPY . .

RUN cd cmd/boringproxy && CGO_ENABLED=0 GOOS=${GOOS} GOARCH=${GOARCH} go build -o boringproxy

FROM scratch
EXPOSE 80 443

COPY --from=builder /build/cmd/boringproxy/boringproxy /

ENTRYPOINT ["/boringproxy"]
CMD ["version"]