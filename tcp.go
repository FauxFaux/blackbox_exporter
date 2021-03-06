// Copyright 2016 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"time"

	"github.com/prometheus/common/log"
)

func dialTCP(target string, w http.ResponseWriter, module Module) (net.Conn, error) {
	var dialProtocol, fallbackProtocol string

	dialer := &net.Dialer{Timeout: module.Timeout}
	if module.TCP.Protocol == "" {
		module.TCP.Protocol = "tcp"
	}
	if module.TCP.Protocol == "tcp" && module.TCP.PreferredIpProtocol == "" {
		module.TCP.PreferredIpProtocol = "ip6"
	}
	if module.TCP.PreferredIpProtocol == "ip6" {
		fallbackProtocol = "ip4"
	} else {
		fallbackProtocol = "ip6"
	}

	dialProtocol = module.TCP.Protocol
	if module.TCP.Protocol == "tcp" {
		target_address, _, err := net.SplitHostPort(target)
		ip, err := net.ResolveIPAddr(module.TCP.PreferredIpProtocol, target_address)
		if err != nil {
			ip, err = net.ResolveIPAddr(fallbackProtocol, target_address)
			if err != nil {
				return nil, err
			}
		}

		if ip.IP.To4() == nil {
			dialProtocol = "tcp6"
		} else {
			dialProtocol = "tcp4"
		}
	}

	if dialProtocol == "tcp6" {
		fmt.Fprintf(w, "probe_ip_protocol 6\n")
	} else {
		fmt.Fprintf(w, "probe_ip_protocol 4\n")
	}

	if !module.TCP.TLS {
		return dialer.Dial(dialProtocol, target)
	}
	config, err := module.TCP.TLSConfig.GenerateConfig()
	if err != nil {
		return nil, err
	}
	if "postgres" == module.TCP.TLSConfig.StartTLS {
		conn, err := dialer.Dial("tcp", target)
		if nil != err {
			return nil, err
		}

		// https://www.postgresql.org/docs/9.5/static/protocol-message-formats.html
		// To start SSL, instead of a normal conversation, the client sends SSLRequest:
		if _, err := conn.Write([]byte{0, 0, 0, 8, 0x04, 0xd2, 0x16, 0x2f}); nil != err {
			return nil, err
		}

		// then one byte is returned, either 'S' for "sure", or 'R' for "nah"
		resp := []byte{0}
		if _, err := conn.Read(resp); nil != err {
			return nil, err
		}
		if 'S' != resp[0] {
			return nil, errors.New("server doesn't want to speak TLS")
		}

		// once this is complete, normal TLS can continue
		client := tls.Client(conn, config)
		if err := client.Handshake(); nil != err {
			return nil, err
		}
		return client, nil
	} else if "" != module.TCP.TLSConfig.StartTLS {
		return nil, errors.New("unrecognised STARTTLS mode")
	}
	return tls.DialWithDialer(dialer, dialProtocol, target, config)
}

func probeTCP(target string, w http.ResponseWriter, module Module) bool {
	deadline := time.Now().Add(module.Timeout)
	conn, err := dialTCP(target, w, module)
	if err != nil {
		return false
	}
	defer conn.Close()

	// Set a deadline to prevent the following code from blocking forever.
	// If a deadline cannot be set, better fail the probe by returning an error
	// now rather than blocking forever.
	if err := conn.SetDeadline(deadline); err != nil {
		return false
	}
	if module.TCP.TLS {
		state := conn.(*tls.Conn).ConnectionState()
		fmt.Fprintf(w, "probe_ssl_earliest_cert_expiry %f\n",
			float64(getEarliestCertExpiry(&state).UnixNano())/1e9)
	}
	scanner := bufio.NewScanner(conn)
	for _, qr := range module.TCP.QueryResponse {
		log.Debugf("Processing query response entry %+v", qr)
		send := qr.Send
		if qr.Expect != "" {
			re, err := regexp.Compile(qr.Expect)
			if err != nil {
				log.Errorf("Could not compile %q into regular expression: %v", qr.Expect, err)
				return false
			}
			var match []int
			// Read lines until one of them matches the configured regexp.
			for scanner.Scan() {
				log.Debugf("read %q\n", scanner.Text())
				match = re.FindSubmatchIndex(scanner.Bytes())
				if match != nil {
					log.Debugf("regexp %q matched %q", re, scanner.Text())
					break
				}
			}
			if scanner.Err() != nil {
				return false
			}
			if match == nil {
				return false
			}
			send = string(re.Expand(nil, []byte(send), scanner.Bytes(), match))
		}
		if send != "" {
			log.Debugf("Sending %q", send)
			if _, err := fmt.Fprintf(conn, "%s\n", send); err != nil {
				return false
			}
		}
	}
	return true
}
