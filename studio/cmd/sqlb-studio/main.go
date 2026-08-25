// Command sqlb-studio serves a read-only, generic browser over a sqlb.json
// manifest. It carries no per-application knowledge — see the studio package
// doc and docs/architecture.md's manifest decision in the parent module.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"

	"github.com/mind-vm/sqlb/studio"
)

func main() {
	manifestPath := flag.String("manifest", "sqlb.json", "path to a sqlb.json manifest")
	addr := flag.String("addr", ":4000", "address to listen on")
	apiBase := flag.String("api", "", "REST API root (e.g. http://localhost:8080); data pages are disabled without it")
	basePath := flag.String("base-path", "", "mount point, e.g. /studio, if this binary sits behind a reverse-proxy subpath; empty serves at the root as before")
	flag.Parse()

	m, err := studio.LoadManifest(*manifestPath)
	if err != nil {
		log.Fatal(err)
	}

	srv, err := studio.NewServer(m, *apiBase, *basePath)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("sqlb studio: %d table(s) from %s, listening on http://localhost%s%s\n", len(m.Tables), *manifestPath, *addr, *basePath)
	log.Fatal(http.ListenAndServe(*addr, srv.Handler()))
}
