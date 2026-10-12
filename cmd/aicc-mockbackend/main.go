// SPDX-License-Identifier: Apache-2.0

// Command aicc-mockbackend serves the business APIs the reference flows call,
// so a bot can be walked end to end without a real CRM behind it. The
// fixtures and their reasoning are in internal/mockbackend.
//
//	go run ./cmd/aicc-mockbackend -addr 127.0.0.1:8770
//	AICC_BOT_BACKEND_BASE=http://127.0.0.1:8770 /tmp/aicc
package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/mockbackend"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8770", "listen address")
	flag.Parse()

	log.Printf("mock backend on http://%s — point AICC_BOT_BACKEND_BASE at it", *addr)
	srv := &http.Server{Addr: *addr, Handler: mockbackend.New(log.Printf), ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
