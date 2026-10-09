// Command gen regenerates the embedded models.dev price catalog.
package main

import (
	"bytes"
	"context"
	"github.com/zzstar101/mytoken/internal/pricing"
	"log"
	"os"
)

func main() {
	var buf bytes.Buffer
	if err := pricing.WriteSnapshot(context.Background(), &buf); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile("snapshot.json", buf.Bytes(), 0644); err != nil {
		log.Fatal(err)
	}
}
