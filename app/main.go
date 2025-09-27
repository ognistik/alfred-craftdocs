package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"sort"
	"strings"

	aw "github.com/deanishe/awgo"
	"github.com/kudrykv/alfred-craftdocs-searchindex/app/config"
	"github.com/kudrykv/alfred-craftdocs-searchindex/app/repository"
	"github.com/kudrykv/alfred-craftdocs-searchindex/app/service"
	"github.com/kudrykv/alfred-craftdocs-searchindex/app/types"
	_ "github.com/mattn/go-sqlite3"
)

func initialize() (*config.Config, *service.BlockService, string, error) {
	cfg, err := config.NewConfig()
	if err != nil {
		return nil, nil, "", fmt.Errorf("get config: %w", err)
	}

	var spaces []repository.Space
	for _, si := range cfg.SearchIndexes() {
		db, err := sql.Open("sqlite3", si.Path())
		if err != nil {
			return nil, nil, "", fmt.Errorf("sql open: %w", err)
		}
		spaces = append(spaces, repository.Space{
			ID: si.SpaceID,
			DB: db,
		})
	}

	blockRepo := repository.NewBlockRepo(spaces...)
	blockService := service.NewBlockService(blockRepo)

	return cfg, blockService, "", nil
}

func flow(ctx context.Context, args []string, allSpaces bool, currentSpaceID string) (*config.Config, []repository.Block, error) {
	cfg, blockService, _, err := initialize()
	if err != nil {
		return nil, nil, fmt.Errorf("initialize: %w", err)
	}

	defer func() { _ = blockService.Close() }()

	blocks, err := blockService.Search(ctx, args, allSpaces, currentSpaceID)
	if err != nil {
		return nil, nil, fmt.Errorf("search: %w", err)
	}

	return cfg, blocks, nil
}

func addCreateNewDocument(wf *aw.Workflow, config *config.Config, args []string) {
	name := strings.Join(args, " ")
	title := fmt.Sprintf("Create %q", name)
	url := fmt.Sprintf("craftdocs://createdocument?spaceId=%s&title=%s&content=&folderId=", config.SearchIndexes()[0].SpaceID, url.PathEscape(name))
	wf.
		NewItem(title).
		UID(title).
		Arg(url).
		Valid(true)
}

func main() {
	wf := aw.New()

	defer wf.SendFeedback()
	defer func() {
		if wf.IsEmpty() {
			wf.NewItem("No results")
		}
	}()

	// Read from Alfred's JSON input or environment variable
	allSpacesStr := os.Getenv("allSpaces")
	if allSpacesStr == "" {
		// Try to read from Alfred's stdin JSON (workflow variables)
		if jsonBytes, err := io.ReadAll(os.Stdin); err == nil {
			var alfredInput struct {
				Variables map[string]string `json:"variables"`
			}
			if json.Unmarshal(jsonBytes, &alfredInput) == nil {
				allSpacesStr = alfredInput.Variables["allSpaces"]
			}
		}
	}
	allSpaces := allSpacesStr == "1"
	log.Printf("Search scope: allSpaces=%t (raw: '%s')", allSpaces, allSpacesStr)

	cfg, blockService, _, err := initialize()
	if err != nil {
		log.Printf("Error initializing: %v", err)
		wf.NewWarningItem("Initialization failed", err.Error())
		return
	}
	defer func() { _ = blockService.Close() }()

	var currentSpaceID string
	if !allSpaces && len(cfg.SearchIndexes()) > 0 {
		currentSpaceID = cfg.SearchIndexes()[0].SpaceID // Primary space
		log.Printf("Using primary space: %s", currentSpaceID)
	} else {
		log.Printf("Searching all spaces")
	}

	config, blocks, err := flow(context.Background(), os.Args[1:], allSpaces, currentSpaceID)
	if err != nil {
		var te types.Error
		if errors.As(err, &te) {
			wf.NewWarningItem(te.Title, err.Error())
		} else {
			wf.NewWarningItem("Unknown error", err.Error())
		}

		return
	}

	if len(blocks) == 0 {
		addCreateNewDocument(wf, config, os.Args[1:])
	}

	// Sort all documents (across spaces) on top, whilst maintaining
	// order, primary space documents will always be on top.
	sort.SliceStable(blocks, func(i, j int) bool {
		if blocks[i].IsDocument() && !blocks[j].IsDocument() {
			return true
		}
		if !blocks[i].IsDocument() && blocks[j].IsDocument() {
			return false
		}
		return i < j
	})

	newDocumentEntryAdded := false
	for _, block := range blocks {
		// Append new document after documents but before
		// individual blocks.
		if !newDocumentEntryAdded && !block.IsDocument() {
			addCreateNewDocument(wf, config, os.Args[1:])
			newDocumentEntryAdded = true
		}
		wf.
			NewItem(block.Content).
			Subtitle(block.DocumentName).
			UID(block.ID).
			Arg("craftdocs://open?blockId=" + block.ID + "&spaceId=" + block.SpaceID).
			Valid(true)
	}
}
