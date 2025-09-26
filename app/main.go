package main

import (
	"context"
	"os"

	aw "github.com/deanishe/awgo"
)

func main() {
	wf := aw.New()

	wf.Run(workflow(context.Background(), wf, os.Args[1:]))
}

//nolint:gochecknoinits
func init() {
	// Use standard sqlite3 driver with FTS5 support
	if len(os.Getenv("alfred_workflow_bundleid")) == 0 {
		if err := os.Setenv("alfred_workflow_bundleid", "dev.kudrykv.craftsearchindex"); err != nil {
			panic(err)
		}
	}

	if len(os.Getenv("alfred_workflow_data")) == 0 {
		if err := os.Setenv("alfred_workflow_data", "./tmp/data"); err != nil {
			panic(err)
		}
	}

	if len(os.Getenv("alfred_workflow_cache")) == 0 {
		if err := os.Setenv("alfred_workflow_cache", "./tmp/cache"); err != nil {
			panic(err)
		}
	}
}
