package main

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"time"
)

func proxyRequest(w http.ResponseWriter, r *http.Request, tunnel Tunnel, httpClient *http.Client, port int) {

	if tunnel.AuthUsername != "" || tunnel.AuthPassword != "" {
		username, password, ok := r.BasicAuth()
		if !ok {
			w.Header()["WWW-Authenticate"] = []string{"Basic"}
			w.WriteHeader(401)
			return
		}

		if username != tunnel.AuthUsername || password != tunnel.AuthPassword {
			w.Header()["WWW-Authenticate"] = []string{"Basic"}
			w.WriteHeader(401)
			// TODO: should probably use a better form of rate limiting
			time.Sleep(2 * time.Second)
			return
		}
	}

	downstreamReqHeaders := r.Header.Clone()

	upstreamAddr := fmt.Sprintf("localhost:%d", port)
	upstreamUrl := fmt.Sprintf("http://%s%s", upstreamAddr, r.URL.RequestURI())

	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		errMessage := fmt.Sprintf("%s", err)
		w.WriteHeader(500)
		io.WriteString(w, errMessage)
		return
	}

	upstreamReq, err := http.NewRequest(r.Method, upstreamUrl, bytes.NewReader(body))
	if err != nil {
		errMessage := fmt.Sprintf("%s", err)
		w.WriteHeader(500)
		io.WriteString(w, errMessage)
		return
	}

	upstreamReq.Header = downstreamReqHeaders

	upstreamReq.Header["X-Forwarded-Host"] = []string{r.Host}
	upstreamReq.Host = fmt.Sprintf("%s:%d", tunnel.ClientAddress, tunnel.ClientPort)

	upstreamRes, err := httpClient.Do(upstreamReq)
	if err != nil {
		errMessage := fmt.Sprintf("%s", err)
		w.WriteHeader(502)
		io.WriteString(w, errMessage)
		return
	}
	defer upstreamRes.Body.Close()

	downstreamResHeaders := w.Header()

	for k, v := range upstreamRes.Header {
		downstreamResHeaders[k] = v
	}

	w.WriteHeader(upstreamRes.StatusCode)
	io.Copy(w, upstreamRes.Body)
}