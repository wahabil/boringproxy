package main

import (
	"encoding/json"
	"io"
	"net/http"
)

type Api struct {
	config *BoringProxyConfig
	auth   *Auth
	tunMan *TunnelManager
	mux    *http.ServeMux
}


func NewApi(config *BoringProxyConfig, auth *Auth, tunMan *TunnelManager) *Api {

	api := &Api{config, auth, tunMan, nil}

	mux := http.NewServeMux()

	mux.Handle("/tunnels", http.StripPrefix("/tunnels", http.HandlerFunc(api.handleTunnels)))

	api.mux = mux

	return api
}

func (a *Api) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mux.ServeHTTP(w, r)
}

func (a *Api) handleTunnels(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		body, err := json.Marshal(a.tunMan.GetTunnels())
		if err != nil {
			w.WriteHeader(500)
			w.Write([]byte("Error encoding tunnels"))
			return
		}
		w.Write([]byte(body))
	case "POST":
		a.validateSession(http.HandlerFunc(a.handleCreateTunnel)).ServeHTTP(w, r)
	case "DELETE":
		a.validateSession(http.HandlerFunc(a.handleDeleteTunnel)).ServeHTTP(w, r)
	default:
		w.WriteHeader(405)
		w.Write([]byte("Invalid method for /tunnels"))
	}
}

func (a *Api) handleCreateTunnel(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	if len(query["domain"]) != 1 {
		w.WriteHeader(400)
		w.Write([]byte("Invalid domain parameter"))
		return
	}
	domain := query["domain"][0]

	tunnel, err := a.tunMan.CreateTunnel(domain)
	if err != nil {
		w.WriteHeader(400)
		io.WriteString(w, err.Error())
		return
	}

	tunnelJson, err := json.MarshalIndent(tunnel, "", "  ")
	if err != nil {
		w.WriteHeader(500)
		io.WriteString(w, "Error encoding tunnel")
		return
	}

	w.Write(tunnelJson)
}

func (a *Api) handleDeleteTunnel(w http.ResponseWriter, r *http.Request) {

	query := r.URL.Query()

	if len(query["domain"]) != 1 {
		w.WriteHeader(400)
		w.Write([]byte("Invalid domain parameter"))
		return
	}
	domain := query["domain"][0]

	err := a.tunMan.DeleteTunnel(domain)
	if err != nil {
		w.WriteHeader(500)
		io.WriteString(w, "Failed to delete tunnel")
		return
	}
}

func (a *Api) validateSession(h http.Handler) http.Handler {

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, err := extractToken("access_token", r)
		if err != nil {
			w.WriteHeader(401)
			w.Write([]byte("No token provided"))
			return
		}

		if !a.auth.Authorized(token) {
			w.WriteHeader(403)
			w.Write([]byte("Not authorized"))
			return
		}

		h.ServeHTTP(w, r)
	})
}
