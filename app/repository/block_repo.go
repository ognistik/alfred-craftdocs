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

// buildMatchQuery creates a FTS5 match query that searches both content
// and exactMatchContent (only set for documents). Content is used for
// regular full-text search and including exactMatchContent allows for
// searches like "mylist" that will match a document named "My List".
//
// The query uses AND to match all terms independently, allowing non-adjacent
// matches. For example, searching "craft workflow" will match "Craft Alfred Workflow"
// because both terms are present, even though not adjacent.
//
// Example output:
//
//	{content exactMatchContent} : ("my" AND "todo") OR ("my"* AND "todo"*) OR ("my" AND "todo"*)
//	{content exactMatchContent} : ("mytodo") OR ("mytodo"*)
func buildMatchQuery(terms []string) string {
	if len(terms) == 0 {
		return ""
	}

	var quotedTerms []string
	for _, term := range terms {
		// Quote the term to avoid FTS5 bareword input sanitization.
		// https://www.sqlite.org/fts5.html#fts5_strings
		term = strings.ReplaceAll(term, "\"", " ")
		quotedTerms = append(quotedTerms, fmt.Sprintf("%q", term))
	}

	// Create different permutations of the match query using AND to allow
	// non-adjacent term matching. We use different combinations to give
	// more weight (rank) to exact matches vs globbed matches:
	// 1. Exact terms (highest rank)
	// 2. Exact terms with last term globbed
	// 3. All terms globbed (catches all matches)
	var matchPhrases []string
	if len(quotedTerms) == 1 {
		// Single term - use simpler query without AND
		matchPhrases = []string{
			quotedTerms[0],       // '"term"'
			quotedTerms[0] + "*", // '"term"*'
		}
	} else {
		// Multiple terms - use AND to allow non-adjacent matching
		// Create globbed versions of terms for partial matching
		var globbedTerms []string
		for _, term := range quotedTerms {
			globbedTerms = append(globbedTerms, term+"*")
		}
		
		// Create a mixed version: all terms globbed for best matching
		// This ensures we match "Craft Workflow" against "Craft Alfred Workflow"
		matchPhrases = []string{
			strings.Join(globbedTerms, " AND "),      // '"term1"* AND "term2"*' (best for non-adjacent)
			strings.Join(quotedTerms, " AND "),       // '"term1" AND "term2"' (exact match, higher rank)
		}
	}

	matchQuery := fmt.Sprintf("{content exactMatchContent} : (%s)", strings.Join(matchPhrases, ") OR ("))

	return matchQuery
}

func (b *BlockRepo) searchWithLike(ctx context.Context, space Space, terms []string, limit int) (*sql.Rows, error) {
	// Build LIKE query for fallback when FTS5 is not available
	// Try multiple table names in case the structure varies
	tableNames := []string{"BlockSearch_content", "BlockSearch"}

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
	matchQuery := buildMatchQuery(terms)
	log.Printf("Searching with matchQuery: '%s'", matchQuery)

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

		var rows *sql.Rows
		var err error
		if len(matchQuery) > 0 {
			// Try FTS5 first, with error handling
			query := `
				SELECT id, content, entityType, documentId
				FROM BlockSearch(?)
				ORDER BY rank + customRank
				LIMIT ?
			`
			rows, err = space.DB.QueryContext(ctx, query, matchQuery, limit)
			if err != nil {
				log.Printf("FTS5 query failed: %v", err)
				errMsg := err.Error()
				if strings.Contains(errMsg, "no such module: fts5") || strings.Contains(errMsg, "no such table") {
					log.Printf("FTS5 not available, falling back to LIKE search")
					rows, err = b.searchWithLike(ctx, space, terms, limit)
					if err != nil {
						log.Printf("LIKE search also failed: %v", err)
						return nil, types.NewError("failed to query database search", err)
					}
				} else {
					return nil, types.NewError("failed to query database search", err)
				}
			}
		} else {
			// No search terms were provided, fallback to listing
			// all results according to whatever custom rank Craft
			// has assigned them. Documents have a much lower
			// customRank than blocks, so with 40+ documents all
			// results will be documents. The results seems to be
			// sorted in an order approximating recently accessed,
			// edited or created.
			query := "SELECT id, content, entityType, documentId FROM BlockSearch ORDER BY customRank LIMIT ?"
			rows, err = space.DB.QueryContext(ctx, query, limit)
			if err != nil {
				// FTS5 not available, use fallback
				if strings.Contains(err.Error(), "no such module: fts5") || strings.Contains(err.Error(), "no such table") {
					log.Printf("FTS5 not available for empty search, falling back to LIKE search")
					rows, err = b.searchWithLike(ctx, space, terms, limit)
					if err != nil {
						log.Printf("LIKE search also failed: %v", err)
						return nil, types.NewError("failed to query database search", err)
					}
				} else {
					return nil, types.NewError("failed to query database", err)
				}
			}
		}
		if err != nil {
			return nil, types.NewError("failed to query database", err)
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

		query := `select documentId, content from BlockSearch where entityType = 'document' and documentId in (` + strings.Join(placeholders, ", ") + ")"
		rows, err := space.DB.QueryContext(ctx, query, ids...)
		if err != nil {
			// Try fallback table if BlockSearch fails (FTS5 not available)
			if strings.Contains(err.Error(), "no such module: fts5") || strings.Contains(err.Error(), "no such table") {
				query = `select c7 as documentId, c1 as content from BlockSearch_content where c3 = 'document' and c7 in (` + strings.Join(placeholders, ", ") + ")"
				rows, err = space.DB.QueryContext(ctx, query, ids...)
				if err != nil {
					return nil, types.NewError("failed to query the database", err)
				}
			} else {
				return nil, types.NewError("failed to query the database", err)
			}
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
