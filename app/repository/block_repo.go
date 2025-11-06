package repository

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/kudrykv/alfred-craftdocs-searchindex/app/types"
)

const (
	searchResultLimit = 40
	// Fetch extra results to account for date filtering
	fetchBuffer = 20
)

type Space struct {
	ID string
	DB *sql.DB
}

type BlockRepo struct {
	spaces []Space
}

func NewBlockRepo(spaces ...Space) *BlockRepo {
	return &BlockRepo{spaces: spaces}
}

func (br *BlockRepo) Close() (err error) {
	for _, space := range br.spaces {
		err2 := space.DB.Close()
		if err == nil {
			err = err2
		}
	}
	return err
}

type Block struct {
	ID           string
	SpaceID      string
	Content      string
	EntityType   string
	DocumentID   string
	DocumentName string
}

func (b *Block) IsDocument() bool {
	return b.EntityType == "document"
}

// isDateTitle checks if the content matches the date pattern YYYY.MM.DD
func isDateTitle(content string) bool {
	if len(content) != 10 {
		return false
	}
	// Check pattern: YYYY.MM.DD
	return content[4] == '.' && content[7] == '.' &&
		isDigits(content[0:4]) && isDigits(content[5:7]) && isDigits(content[8:10])
}

// isDigits checks if all characters in the string are digits
func isDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// filterDateTitles removes documents with date-like titles and returns exactly searchResultLimit items
// If daily is true, date-titled documents are included in results
func (b *BlockRepo) filterDateTitles(blocks []Block, daily bool) []Block {
	filtered := make([]Block, 0, len(blocks))

	for _, block := range blocks {
		// Skip documents with date-like titles only if daily is false
		if !daily && block.IsDocument() && isDateTitle(block.Content) {
			continue
		}
		filtered = append(filtered, block)

		// Stop once we have enough results
		if len(filtered) >= searchResultLimit {
			break
		}
	}

	return filtered
}



func (b *BlockRepo) searchWithLike(ctx context.Context, space Space, terms []string, limit int) (*sql.Rows, error) {
	// Build LIKE query for searching content
	// Try multiple table names in case the structure varies
	tableNames := []string{"BlockSearch_content"}

	for _, tableName := range tableNames {
		var query string
		var args []interface{}

		if len(terms) == 0 {
			// No search terms, return all results ordered by some criteria
			query = fmt.Sprintf(`
				SELECT c0 as id, c1 as content, c3 as entityType, c7 as documentId 
				FROM %s 
				ORDER BY c0 
				LIMIT ?
			`, tableName)
			args = []interface{}{limit}
		} else {
			conditions := make([]string, 0, len(terms))
			args = make([]interface{}, 0, len(terms)+1)

			for _, term := range terms {
				conditions = append(conditions, "c1 LIKE ?") // c1 contains the content
				args = append(args, "%"+term+"%")
			}

			whereClause := strings.Join(conditions, " AND ")
			query = fmt.Sprintf(`
				SELECT c0 as id, c1 as content, c3 as entityType, c7 as documentId 
				FROM %s 
				WHERE %s 
				LIMIT ?
			`, tableName, whereClause)
			args = append(args, limit)
		}

		log.Printf("Trying LIKE query on %s: %s, args: %v", tableName, query, args)

		rows, err := space.DB.QueryContext(ctx, query, args...)
		if err == nil {
			return rows, nil
		}
		log.Printf("LIKE query on %s failed: %v", tableName, err)
	}

	// If both table attempts fail, try a simpler approach
	log.Printf("All LIKE queries failed, trying basic search")
	return space.DB.QueryContext(ctx, "SELECT c0 as id, c1 as content, c3 as entityType, c7 as documentId FROM BlockSearch_content LIMIT ?", limit)
}

func (b *BlockRepo) Search(ctx context.Context, terms []string, allSpaces bool, daily bool, currentSpaceID string) ([]Block, error) {
	log.Printf("Searching with terms: %v", terms)

	blocks := make([]Block, 0, searchResultLimit)

	// Filter spaces based on allSpaces and currentSpaceID
	var spacesToSearch []Space
	if allSpaces {
		spacesToSearch = b.spaces
	} else if currentSpaceID != "" {
		// Only search the specified primary space
		for _, space := range b.spaces {
			if space.ID == currentSpaceID {
				spacesToSearch = []Space{space}
				break
			}
		}
		if len(spacesToSearch) == 0 {
			log.Printf("Primary space %s not found, searching all spaces", currentSpaceID)
			spacesToSearch = b.spaces
		}
	} else {
		spacesToSearch = b.spaces
	}

	for _, space := range spacesToSearch {
		// Fetch extra results to account for date filtering
		limit := searchResultLimit + fetchBuffer - len(blocks)
		if limit <= fetchBuffer {
			limit = searchResultLimit + fetchBuffer
		}
		if limit == fetchBuffer && len(blocks) >= searchResultLimit {
			break
		}
		log.Printf("Searching %s, limit %d", space.ID, limit)

		// Use LIKE-based search for all queries
		rows, err := b.searchWithLike(ctx, space, terms, limit)
		if err != nil {
			log.Printf("LIKE search failed: %v", err)
			return nil, types.NewError("failed to query database search", err)
		}

		for rows.Next() {
			block := Block{SpaceID: space.ID}

			if err = rows.Scan(&block.ID, &block.Content, &block.EntityType, &block.DocumentID); err != nil {
				return nil, types.NewError("failed to scan a row", err)
			}

			blocks = append(blocks, block)
		}

		if err = rows.Err(); err != nil {
			return nil, types.NewError("error in rows", err)
		}

		if err = rows.Close(); err != nil {
			return nil, types.NewError("closing rows failed", err)
		}
	}

	return b.filterDateTitles(blocks, daily), nil
}

type docKey struct {
	spaceID string
	docID   string
}

func (b *BlockRepo) BackfillDocumentNames(ctx context.Context, blocks []Block, targetSpaceIDs map[string]struct{}) ([]Block, error) {
	if len(blocks) == 0 {
		return blocks, nil
	}

	blocksBySpace := make(map[string][]Block)
	for _, block := range blocks {
		blocksBySpace[block.SpaceID] = append(blocksBySpace[block.SpaceID], block)
	}

	docIDs := make(map[docKey]string)

	for _, space := range b.spaces {
		b := blocksBySpace[space.ID]

		ids := make([]interface{}, 0, len(b))
		placeholders := make([]string, 0, len(ids))
		for _, k := range b {
			if k.IsDocument() {
				// This is a document, no need to fetch title.
				continue
			}
			ids = append(ids, k.DocumentID)
			placeholders = append(placeholders, "?"+strconv.Itoa(len(ids)))
		}

		// Use BlockSearch_content table directly (no FTS5)
		query := `select c7 as documentId, c1 as content from BlockSearch_content where c3 = 'document' and c7 in (` + strings.Join(placeholders, ", ") + ")"
		rows, err := space.DB.QueryContext(ctx, query, ids...)
		if err != nil {
			return nil, types.NewError("failed to query the database", err)
		}

		for rows.Next() {
			var block Block

			if err = rows.Scan(&block.DocumentID, &block.Content); err != nil {
				return nil, types.NewError("failed to scan row", err)
			}

			docIDs[docKey{spaceID: space.ID, docID: block.DocumentID}] = block.Content
		}

		if err = rows.Err(); err != nil {
			return nil, types.NewError("error in rows", err)
		}

		if err = rows.Close(); err != nil {
			return nil, types.NewError("closing rows failed", err)
		}
	}

	// Avoid mutating data in original slice.
	backfilled := make([]Block, len(blocks))
	copy(backfilled, blocks)

	for i, block := range backfilled {
		if block.IsDocument() {
			backfilled[i].DocumentName = "[Document]"
		} else {
			backfilled[i].DocumentName = "[Block] " + docIDs[docKey{spaceID: block.SpaceID, docID: block.DocumentID}]
		}
	}

	return backfilled, nil
}
