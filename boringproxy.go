package boringproxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"golang.org/x/oauth2"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/mdp/qrterminal/v3"

	"github.com/takingnames/namedrop-go"
)

type Config struct {
	SshServerPort int    `json:"ssh_server_port"`
	PublicIp      string `json:"public_ip"`
	oauth2Config  *oauth2.Config
}

type SmtpConfig struct {
	Server   string
	Port     int
	Username string
	Password string
}

type Server struct {
	db           *Database
	tunMan       *TunnelManager
	httpClient   *http.Client
	httpListener *PassthroughListener
}

func checkPublicAddress(host string, port int) error {

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return err
	}
	defer ln.Close()

	code, err := genRandomCode(32)
	if err != nil {
		return err
	}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				break
			}
			conn.Write([]byte(code))
			conn.Close()
		}
	}()

	addr := fmt.Sprintf("%s:%d", host, port)
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()

	go func() {
		time.Sleep(time.Second)
		conn.Close()
	}()

	data, err := io.ReadAll(conn)
	if err != nil {
		return errors.New(fmt.Sprintf("Error connecting to public address %s. Probably timed out", addr))
	}

	retCode := string(data)

	if retCode != code {
		return errors.New("Mismatched codes")
	}

	return nil
}

func getPublicIp() (string, error) {
	resp, err := http.Get("https://takingnames.io/my-ip")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)

	return string(body), nil
}

func Listen() {
	flagSet := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	newAdminDomain := flagSet.String("admin-domain", "", "Admin Domain")
	sshServerPort := flagSet.Int("ssh-server-port", 22, "SSH Server Port")
	certDir := flagSet.String("cert-dir", "", "TLS cert directory")
	printLogin := flagSet.Bool("print-login", false, "Prints admin login information")
	httpPort := flagSet.Int("http-port", 80, "HTTP (insecure) port")
	httpsPort := flagSet.Int("https-port", 443, "HTTPS (secure) port")
	allowHttp := flagSet.Bool("allow-http", false, "Allow unencrypted (HTTP) requests")
	err := flagSet.Parse(os.Args[2:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: parsing flags: %s\n", os.Args[0], err)
	}

	log.Println("Starting up")

	ip, err := getPublicIp()
	if err != nil {
		log.Fatal(err)
	}

	err = checkPublicAddress(ip, *httpPort)
	if err != nil {
		log.Fatal(err)
	}

	err = checkPublicAddress(ip, *httpsPort)
	if err != nil {
		log.Fatal(err)
	}

	db, err := NewDatabase()
	if err != nil {
		log.Fatal(err)
	}

	if *certDir != "" {
		certmagic.Default.Storage = &certmagic.FileStorage{*certDir}
	}
	//certmagic.DefaultACME.DisableHTTPChallenge = true
	//certmagic.DefaultACME.DisableTLSALPNChallenge = true
	//certmagic.DefaultACME.CA = certmagic.LetsEncryptStagingCA
	certConfig := certmagic.NewDefault()

	if *newAdminDomain != "" {
		db.SetAdminDomain(*newAdminDomain)
	}

	adminDomain := db.GetAdminDomain()

	if adminDomain == "" {

		err = setAdminDomain(ip, certConfig, db)
		if err != nil {
			log.Fatal(err)
		}
	} else {
		err = certConfig.ManageSync([]string{adminDomain})
		if err != nil {
			log.Fatal(err)
		}
		log.Print(fmt.Sprintf("Successfully acquired certificate for admin domain (%s)", adminDomain))
	}

	// Add admin user if it doesn't already exist
	users := db.GetUsers()
	if len(users) == 0 {
		db.AddUser("admin", true)
		_, err := db.AddToken("admin")
		if err != nil {
			log.Fatal("Failed to initialize admin user")
		}

	}

	if *printLogin {
		tokens := db.GetTokens()

		for token, tokenData := range tokens {
			if tokenData.Owner == "admin" {
				printLoginInfo(token, db.GetAdminDomain())
				break
			}
		}
	}

	oauthConf := &oauth2.Config{
		ClientID:     db.GetAdminDomain(),
		ClientSecret: "fake-secret",
		Scopes:       []string{"subdomain"},
		Endpoint: oauth2.Endpoint{
			AuthURL:  "https://takingnames.io/namedrop/authorize",
			TokenURL: "https://takingnames.io/namedrop/token",
		},
		RedirectURL: fmt.Sprintf("%s/namedrop/auth-success", db.GetAdminDomain()),
	}

	config := &Config{
		SshServerPort: *sshServerPort,
		PublicIp:      ip,
		oauth2Config:  oauthConf,
	}

	tunMan := NewTunnelManager(config, db, certConfig)

	auth := NewAuth(db)

	api := NewApi(config, db, auth, tunMan)

	webUiHandler := NewWebUiHandler(config, db, api, auth)

	httpClient := &http.Client{
		// Don't follow redirects
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	httpListener := NewPassthroughListener()

	p := &Server{db, tunMan, httpClient, httpListener}

	tlsConfig := &tls.Config{
		GetCertificate: certConfig.GetCertificate,
		NextProtos:     []string{"h2", "acme-tls/1"},
	}
	tlsListener := tls.NewListener(httpListener, tlsConfig)

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		timestamp := time.Now().Format(time.RFC3339)
		srcIp := strings.Split(r.RemoteAddr, ":")[0]
		fmt.Println(fmt.Sprintf("%s %s %s %s %s", timestamp, srcIp, r.Method, r.Host, r.URL.Path))

		// TODO: handle ipv6
		hostParts := strings.Split(r.Host, ":")
		hostDomain := hostParts[0]

		if r.URL.Path == "/dnsapi/requests" {
			r.ParseForm()

			requestId := r.Form.Get("request-id")

			dnsRequest, err := db.GetDNSRequest(requestId)
			if err != nil {
				w.WriteHeader(500)
				io.WriteString(w, err.Error())
				return
			}

			jsonBytes, err := json.Marshal(dnsRequest)
			if err != nil {
				w.WriteHeader(500)
				io.WriteString(w, err.Error())
				return
			}

			w.Write(jsonBytes)

		} else if r.URL.Path == "/namedrop/auth-success" {
			r.ParseForm()

			requestId := r.Form.Get("state")

			code := r.Form.Get("code")

			// Ensure the request exists
			dnsRequest, err := db.GetDNSRequest(requestId)
			if err != nil {
				w.WriteHeader(500)
				io.WriteString(w, err.Error())
				return
			}

			db.DeleteDNSRequest(requestId)

			ctx := context.Background()
			tok, err := oauthConf.Exchange(ctx, code)
			if err != nil {
				fmt.Println(err.Error())
			}

			accessToken := tok.AccessToken

			tokenResp, err := http.Get("https://takingnames.io/namedrop/token-data?access_token=" + accessToken)
			if err != nil {
				w.WriteHeader(500)
				io.WriteString(w, err.Error())
				return
			}
			defer tokenResp.Body.Close()
			bodyJson, err := io.ReadAll(tokenResp.Body)

			var namedropTokenData namedrop.TokenData
			err = json.Unmarshal(bodyJson, &namedropTokenData)
			if err != nil {
				w.WriteHeader(500)
				io.WriteString(w, err.Error())
				return
			}

			if len(namedropTokenData.Scopes) < 1 {
				w.WriteHeader(500)
				io.WriteString(w, "No scopes returned")
				return
			}

			domain := namedropTokenData.Scopes[0].Domain
			host := namedropTokenData.Scopes[0].Host

			createRecordReq := namedrop.Record{
				Domain: domain,
				Host:   host,
				Type:   "A",
				Value:  config.PublicIp,
				TTL:    300,
			}

			createRecordReqJson, err := json.Marshal(createRecordReq)
			if err != nil {
				w.WriteHeader(500)
				io.WriteString(w, err.Error())
				return
			}

			url := "https://takingnames.io/namedrop/records?access_token=" + accessToken
			req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(createRecordReqJson))
			if err != nil {
				w.WriteHeader(500)
				io.WriteString(w, err.Error())
				return
			}

			resp, err := httpClient.Do(req)
			if err != nil {
				w.WriteHeader(500)
				io.WriteString(w, err.Error())
				return
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				w.WriteHeader(500)
				io.WriteString(w, err.Error())
				return
			}

			fmt.Println(string(body))

			if resp.StatusCode != 200 {
				w.WriteHeader(500)
				io.WriteString(w, "Invalid status code")
				return
			}

			fqdn := host + "." + domain

			if dnsRequest.IsAdminDomain {
				db.SetAdminDomain(fqdn)

				// TODO: Might want to get all certs here, not just the admin domain
				err := certConfig.ManageSync([]string{fqdn})
				if err != nil {
					log.Fatal(err)
				}

				http.Redirect(w, r, fmt.Sprintf("https://%s", fqdn), 303)
			} else {
				adminDomain := db.GetAdminDomain()
				http.Redirect(w, r, fmt.Sprintf("https://%s/edit-tunnel?domain=%s", adminDomain, fqdn), 303)
			}

		} else if r.URL.Path == "/dnsapi/failure" {

			r.ParseForm()

			requestId := r.Form.Get("request-id")

			// Ensure the request exists
			_, err := db.GetDNSRequest(requestId)
			if err != nil {
				w.WriteHeader(500)
				io.WriteString(w, err.Error())
				return
			}

			db.DeleteDNSRequest(requestId)

			http.Redirect(w, r, "/alert?message=Domain request failed", 303)

		} else if hostDomain == db.GetAdminDomain() {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				http.StripPrefix("/api", api).ServeHTTP(w, r)
			} else {
				webUiHandler.handleWebUiRequest(w, r)
			}
		} else {

			tunnel, exists := db.GetTunnel(hostDomain)
			if !exists {
				errMessage := fmt.Sprintf("No tunnel attached to %s", hostDomain)
				w.WriteHeader(500)
				io.WriteString(w, errMessage)
				return
			}

			proxyRequest(w, r, tunnel, httpClient, tunnel.TunnelPort)
		}
	})

	go func() {

		if *allowHttp {
			if err := http.ListenAndServe(fmt.Sprintf(":%d", *httpPort), nil); err != nil {
				log.Fatalf("ListenAndServe error: %v", err)
			}
		} else {
			redirectTLS := func(w http.ResponseWriter, r *http.Request) {
				url := fmt.Sprintf("https://%s:%d%s", r.Host, *httpsPort, r.RequestURI)
				http.Redirect(w, r, url, http.StatusMovedPermanently)
			}

			if err := http.ListenAndServe(fmt.Sprintf(":%d", *httpPort), http.HandlerFunc(redirectTLS)); err != nil {
				log.Fatalf("ListenAndServe error: %v", err)
			}
		}

	}()

	go http.Serve(tlsListener, nil)

	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", *httpsPort))
	if err != nil {
		log.Fatal(err)
	}

	log.Println("Ready")

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Print(err)
			continue
		}

		go p.handleConnection(conn)
	}
}

func (p *Server) handleConnection(clientConn net.Conn) {

	clientHello, clientReader, err := peekClientHello(clientConn)
	if err != nil {
		log.Println("peekClientHello error", err)
		return
	}

	passConn := NewProxyConn(clientConn, clientReader)

	tunnel, exists := p.db.GetTunnel(clientHello.ServerName)

	if exists && (tunnel.TlsTermination == "client" || tunnel.TlsTermination == "passthrough") {
		p.passthroughRequest(passConn, tunnel)
	} else {
		p.httpListener.PassConn(passConn)
	}
}

func (p *Server) passthroughRequest(conn net.Conn, tunnel Tunnel) {

	upstreamAddr := fmt.Sprintf("localhost:%d", tunnel.TunnelPort)
	upstreamConn, err := net.Dial("tcp", upstreamAddr)

	if err != nil {
		log.Print(err)
		return
	}
	defer upstreamConn.Close()

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		io.Copy(conn, upstreamConn)
		conn.(*ProxyConn).CloseWrite()
		wg.Done()
	}()
	go func() {
		io.Copy(upstreamConn, conn)
		upstreamConn.(*net.TCPConn).CloseWrite()
		wg.Done()
	}()

	wg.Wait()
}

func setAdminDomain(ip string, certConfig *certmagic.Config, db *Database) error {
	action := prompt("\nNo admin domain set. Enter '1' to input manually, or '2' to configure through TakingNames.io\n")
	switch action {
	case "1":
		adminDomain := prompt("\nEnter admin domain:\n")

		err := certConfig.ManageSync([]string{adminDomain})
		if err != nil {
			log.Fatal(err)
		}

		db.SetAdminDomain(adminDomain)
	case "2":

		log.Println("Get bootstrap domain")

		resp, err := http.Get("https://takingnames.io/dnsapi/bootstrap-domain")
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)

		bootstrapDomain := string(body)

		if resp.StatusCode != 200 {
			fmt.Println(bootstrapDomain)
			return errors.New("bootstrap domain request failed")
		}

		log.Println("Get cert")

		err = certConfig.ManageSync([]string{bootstrapDomain})
		if err != nil {
			log.Fatal(err)
		}

		requestId, _ := genRandomCode(32)

		req := DNSRequest{
			IsAdminDomain: true,
			Records: []*DNSRecord{
				&DNSRecord{
					Type:  "A",
					Value: ip,
					TTL:   300,
				},
			},
		}

		db.SetDNSRequest(requestId, req)

		tnLink := fmt.Sprintf("https://takingnames.io/dnsapi?requester=%s&request-id=%s", bootstrapDomain, requestId)
		fmt.Println("Use the link below to select an admin domain:\n\n" + tnLink + "\n")

	default:
		log.Fatal("Invalid option")
	}

	return nil
}

func prompt(promptText string) string {
	reader := bufio.NewReader(os.Stdin)
	fmt.Print(promptText)
	text, _ := reader.ReadString('\n')
	return strings.TrimSpace(text)
}

func printLoginInfo(token, adminDomain string) {
	log.Println("Admin token: " + token)
	url := fmt.Sprintf("https://%s/login?access_token=%s", adminDomain, token)
	log.Println(fmt.Sprintf("Admin login link: %s", url))
	qrterminal.GenerateHalfBlock(url, qrterminal.L, os.Stdout)
}
