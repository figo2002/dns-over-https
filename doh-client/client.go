/*
   DNS-over-HTTPS
   Copyright (C) 2017-2018 Star Brilliant <m13253@hotmail.com>

   Permission is hereby granted, free of charge, to any person obtaining a
   copy of this software and associated documentation files (the "Software"),
   to deal in the Software without restriction, including without limitation
   the rights to use, copy, modify, merge, publish, distribute, sublicense,
   and/or sell copies of the Software, and to permit persons to whom the
   Software is furnished to do so, subject to the following conditions:

   The above copyright notice and this permission notice shall be included in
   all copies or substantial portions of the Software.

   THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
   IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
   FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
   AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
   LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING
   FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER
   DEALINGS IN THE SOFTWARE.
*/

package main

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/http/cookiejar"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/m13253/dns-over-https/json-dns"
	"github.com/miekg/dns"
	"golang.org/x/net/http2"
	"golang.org/x/net/idna"
)

type Client struct {
	conf                 *config
	bootstrap            []string
	passthrough          []string
	udpClient            *dns.Client
	tcpClient            *dns.Client
	udpServers           []*dns.Server
	tcpServers           []*dns.Server
	bootstrapResolver    *net.Resolver
	cookieJar            http.CookieJar
	httpClientMux        *sync.RWMutex
	httpTransport        *http.Transport
	httpClient           *http.Client
	httpClientLastCreate time.Time
}

type DNSRequest struct {
	response          *http.Response
	reply             *dns.Msg
	udpSize           uint16
	ednsClientAddress net.IP
	ednsClientNetmask uint8
	currentUpstream   string
	err               error
}

func NewClient(conf *config) (c *Client, err error) {
	c = &Client{
		conf: conf,
	}

	udpHandler := dns.HandlerFunc(c.udpHandlerFunc)
	tcpHandler := dns.HandlerFunc(c.tcpHandlerFunc)
	c.udpClient = &dns.Client{
		Net:     "udp",
		UDPSize: dns.DefaultMsgSize,
		Timeout: time.Duration(conf.Timeout) * time.Second,
	}
	c.tcpClient = &dns.Client{
		Net:     "tcp",
		Timeout: time.Duration(conf.Timeout) * time.Second,
	}
	for _, addr := range conf.Listen {
		c.udpServers = append(c.udpServers, &dns.Server{
			Addr:    addr,
			Net:     "udp",
			Handler: udpHandler,
			UDPSize: dns.DefaultMsgSize,
		})
		c.tcpServers = append(c.tcpServers, &dns.Server{
			Addr:    addr,
			Net:     "tcp",
			Handler: tcpHandler,
		})
	}
	c.bootstrapResolver = net.DefaultResolver
	if len(conf.Bootstrap) != 0 {
		c.bootstrap = make([]string, len(conf.Bootstrap))
		for i, bootstrap := range conf.Bootstrap {
			bootstrapAddr, err := net.ResolveUDPAddr("udp", bootstrap)
			if err != nil {
				bootstrapAddr, err = net.ResolveUDPAddr("udp", "["+bootstrap+"]:53")
			}
			if err != nil {
				return nil, err
			}
			c.bootstrap[i] = bootstrapAddr.String()
		}
		c.bootstrapResolver = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				var d net.Dialer
				numServers := len(c.bootstrap)
				bootstrap := c.bootstrap[rand.Intn(numServers)]
				conn, err := d.DialContext(ctx, network, bootstrap)
				return conn, err
			},
		}
		if len(conf.Passthrough) != 0 {
			c.passthrough = make([]string, len(conf.Passthrough))
			for i, passthrough := range conf.Passthrough {
				if punycode, err := idna.ToASCII(passthrough); err != nil {
					passthrough = punycode
				}
				c.passthrough[i] = "." + strings.ToLower(strings.Trim(passthrough, ".")) + "."
			}
		}
	}
	// Most CDNs require Cookie support to prevent DDoS attack.
	// Disabling Cookie does not effectively prevent tracking,
	// so I will leave it on to make anti-DDoS services happy.
	if !c.conf.NoCookies {
		c.cookieJar, err = cookiejar.New(nil)
		if err != nil {
			return nil, err
		}
	} else {
		c.cookieJar = nil
	}

	c.httpClientMux = new(sync.RWMutex)
	err = c.newHTTPClient()
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Client) newHTTPClient() error {
	c.httpClientMux.Lock()
	defer c.httpClientMux.Unlock()
	if !c.httpClientLastCreate.IsZero() && time.Now().Sub(c.httpClientLastCreate) < time.Duration(c.conf.Timeout)*time.Second {
		return nil
	}
	if c.httpTransport != nil {
		c.httpTransport.CloseIdleConnections()
	}
	dialer := &net.Dialer{
		Timeout:   time.Duration(c.conf.Timeout) * time.Second,
		KeepAlive: 30 * time.Second,
		DualStack: true,
		Resolver:  c.bootstrapResolver,
	}
	c.httpTransport = &http.Transport{
		DialContext:           dialer.DialContext,
		ExpectContinueTimeout: 1 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		Proxy:                 http.ProxyFromEnvironment,
		TLSHandshakeTimeout:   time.Duration(c.conf.Timeout) * time.Second,
	}
	if c.conf.NoIPv6 {
		c.httpTransport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			if strings.HasPrefix(network, "tcp") {
				network = "tcp4"
			}
			return dialer.DialContext(ctx, network, address)
		}
	}
	err := http2.ConfigureTransport(c.httpTransport)
	if err != nil {
		return err
	}
	c.httpClient = &http.Client{
		Transport: c.httpTransport,
		Jar:       c.cookieJar,
	}
	c.httpClientLastCreate = time.Now()
	return nil
}

func (c *Client) Start() error {
	results := make(chan error, len(c.udpServers)+len(c.tcpServers))
	for _, srv := range append(c.udpServers, c.tcpServers...) {
		go func(srv *dns.Server) {
			err := srv.ListenAndServe()
			if err != nil {
				log.Println(err)
			}
			results <- err
		}(srv)
	}

	for i := 0; i < cap(results); i++ {
		err := <-results
		if err != nil {
			return err
		}
	}
	close(results)
	return nil
}

func (c *Client) handlerFunc(w dns.ResponseWriter, r *dns.Msg, isTCP bool) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(c.conf.Timeout)*time.Second)
	defer cancel()

	if r.Response == true {
		log.Println("Received a response packet")
		return
	}

	if len(r.Question) != 1 {
		log.Println("Number of questions is not 1")
		reply := jsonDNS.PrepareReply(r)
		reply.Rcode = dns.RcodeFormatError
		w.WriteMsg(reply)
		return
	}
	question := &r.Question[0]
	questionName := question.Name
	questionClass := ""
	if qclass, ok := dns.ClassToString[question.Qclass]; ok {
		questionClass = qclass
	} else {
		questionClass = strconv.FormatUint(uint64(question.Qclass), 10)
	}
	questionType := ""
	if qtype, ok := dns.TypeToString[question.Qtype]; ok {
		questionType = qtype
	} else {
		questionType = strconv.FormatUint(uint64(question.Qtype), 10)
	}
	if c.conf.Verbose {
		fmt.Printf("%s - - [%s] \"%s %s %s\"\n", w.RemoteAddr(), time.Now().Format("02/Jan/2006:15:04:05 -0700"), questionName, questionClass, questionType)
	}

	shouldPassthrough := false
	passthroughQuestionName := questionName
	if punycode, err := idna.ToASCII(passthroughQuestionName); err != nil {
		passthroughQuestionName = punycode
	}
	passthroughQuestionName = "." + strings.ToLower(strings.Trim(passthroughQuestionName, ".")) + "."
	for _, passthrough := range c.passthrough {
		if strings.HasSuffix(passthroughQuestionName, passthrough) {
			shouldPassthrough = true
			break
		}
	}
	if shouldPassthrough {
		numServers := len(c.bootstrap)
		upstream := c.bootstrap[rand.Intn(numServers)]
		log.Printf("Request \"%s %s %s\" is passed through %s.\n", questionName, questionClass, questionType, upstream)
		var reply *dns.Msg
		var err error
		if !isTCP {
			reply, _, err = c.udpClient.Exchange(r, upstream)
		} else {
			reply, _, err = c.tcpClient.Exchange(r, upstream)
		}
		if err == nil || err == dns.ErrTruncated {
			w.WriteMsg(reply)
			return
		}
		log.Println(err)
		reply = jsonDNS.PrepareReply(r)
		reply.Rcode = dns.RcodeServerFailure
		w.WriteMsg(reply)
		return
	}

	requestType := ""
	if len(c.conf.UpstreamIETF) == 0 {
		requestType = "application/dns-json"
	} else if len(c.conf.UpstreamGoogle) == 0 {
		requestType = "application/dns-message"
	} else {
		numServers := len(c.conf.UpstreamGoogle) + len(c.conf.UpstreamIETF)
		random := rand.Intn(numServers)
		if random < len(c.conf.UpstreamGoogle) {
			requestType = "application/dns-json"
		} else {
			requestType = "application/dns-message"
		}
	}

	var req *DNSRequest
	if requestType == "application/dns-json" {
		req = c.generateRequestGoogle(ctx, w, r, isTCP)
	} else if requestType == "application/dns-message" {
		req = c.generateRequestIETF(ctx, w, r, isTCP)
	} else {
		panic("Unknown request Content-Type")
	}

	if req.response != nil {
		defer req.response.Body.Close()
		for _, header := range c.conf.DebugHTTPHeaders {
			if value := req.response.Header.Get(header); value != "" {
				log.Printf("%s: %s\n", header, value)
			}
		}
	}
	if req.err != nil {
		return
	}

	contentType := ""
	candidateType := strings.SplitN(req.response.Header.Get("Content-Type"), ";", 2)[0]
	if candidateType == "application/json" {
		contentType = "application/json"
	} else if candidateType == "application/dns-message" {
		contentType = "application/dns-message"
	} else if candidateType == "application/dns-udpwireformat" {
		contentType = "application/dns-message"
	} else {
		if requestType == "application/dns-json" {
			contentType = "application/json"
		} else if requestType == "application/dns-message" {
			contentType = "application/dns-message"
		}
	}

	if contentType == "application/json" {
		c.parseResponseGoogle(ctx, w, r, isTCP, req)
	} else if contentType == "application/dns-message" {
		c.parseResponseIETF(ctx, w, r, isTCP, req)
	} else {
		panic("Unknown response Content-Type")
	}
}

func (c *Client) udpHandlerFunc(w dns.ResponseWriter, r *dns.Msg) {
	c.handlerFunc(w, r, false)
}

func (c *Client) tcpHandlerFunc(w dns.ResponseWriter, r *dns.Msg) {
	c.handlerFunc(w, r, true)
}

var (
	ipv4Mask24 = net.IPMask{255, 255, 255, 0}
	ipv6Mask56 = net.CIDRMask(56, 128)
)

func (c *Client) findClientIP(w dns.ResponseWriter, r *dns.Msg) (ednsClientAddress net.IP, ednsClientNetmask uint8) {
	ednsClientNetmask = 255
	if c.conf.NoECS {
		return net.IPv4(0, 0, 0, 0), 0
	}
	if opt := r.IsEdns0(); opt != nil {
		for _, option := range opt.Option {
			if option.Option() == dns.EDNS0SUBNET {
				edns0Subnet := option.(*dns.EDNS0_SUBNET)
				ednsClientAddress = edns0Subnet.Address
				ednsClientNetmask = edns0Subnet.SourceNetmask
				return
			}
		}
	}
	remoteAddr, err := net.ResolveUDPAddr("udp", w.RemoteAddr().String())
	if err != nil {
		return
	}
	if ip := remoteAddr.IP; jsonDNS.IsGlobalIP(ip) {
		if ipv4 := ip.To4(); ipv4 != nil {
			ednsClientAddress = ipv4.Mask(ipv4Mask24)
			ednsClientNetmask = 24
		} else {
			ednsClientAddress = ip.Mask(ipv6Mask56)
			ednsClientNetmask = 56
		}
	}
	return
}
