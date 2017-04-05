// Copyright 2016 Apcera Inc. All right reserved.

package main

import (
	"fmt"
	"os"
	"time"

	"github.com/apcera/util/events"
)

func main() {
	bearerToken := os.Getenv("CNTM_TOKEN")
	if bearerToken == "" {
		fmt.Fprintln(os.Stderr, "No bearer token provided via $CNTM_TOKEN")
		os.Exit(1)
	}

	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: `CNTM_TOKEN=\"Bearer ...\" events_main https://api.<cluster>/v1/wamp <job-fqn>")
		os.Exit(1)
	}

	wampServerURL := os.Args[1]
	streamFQN := os.Args[2]

	fmt.Printf("Creating WAMP client against %q with token %q...\n", wampServerURL, bearerToken)

	client, err := events.NewWAMPSessionClient(wampServerURL, bearerToken, "com.apcera.api.es")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create WAMP client: %s\n", err)
		os.Exit(1)
	}

	fmt.Printf("Done; streaming %q...\n\n", streamFQN)
	if err := client.StreamEvents(os.Stdout, streamFQN, time.Minute); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to stream %q: %s\n", streamFQN, err)
		os.Exit(1)
	}
}
