// Command fakeregistry serves fake Ollama models for the local dev cluster,
// so trying the dashboard never downloads real models.
package main

import (
	"flag"
	"log"
	"net/http"

	"aicluster/test/fakeregistry"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:7470", "listen address")
	size := flag.Int64("layer-mb", 24, "size of each fake model in MB")
	speed := flag.Int64("mbps", 6, "download speed in MB/s (0 = unlimited)")
	flag.Parse()
	r := fakeregistry.New(*size << 20)
	r.BytesPerSec = *speed << 20
	log.Printf("fake registry on http://%s (%d MB models at %d MB/s)", *listen, *size, *speed)
	log.Fatal(http.ListenAndServe(*listen, r))
}
