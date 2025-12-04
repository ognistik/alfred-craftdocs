package repository

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"

	"github.com/kudrykv/alfred-craftdocs-searchindex/app/types"
)

const (
	// Fetch more results for better fuzzy matching (similar to Bear workflow)
	searchFetchLimit = 1000
	// Display limit for final results
	searchResultLimit = 40
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

type blockRecord struct {
	block                        Block
	isDocument                   bool
	exactMatch                   bool // title contains exact search phrase
	prefixMatch                  bool // title starts with the search phrase
	orderedWordsMatch            bool // title contains all words in order
	allWordsMatch                bool // title contains all words (any order)
	acronymMatch                 bool // "ytv" vs "yt video"
	documentTitleRelevant        bool // true if this is a document and its title contains any of the search words
	containsAnySearchWord        bool // true if this block's content contains at least one of the search words
	parentDocumentID             string // Stores the DocumentID of the block's parent document (or itself if it's a document block)
	parentDocumentWasTopRanked  bool   // true if its parent (or itself if a document) was identified as highly relevant
	docAndBlockAllWordsMatch    bool   // true if document title + this block together contain all search words
	originalIndex                int
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

// containsOrderedWords checks if text contains all words in the given order
func containsOrderedWords(text string, words []string) bool {
	prevPos := 0
	for _, word := range words {
		pos := strings.Index(text[prevPos:], word)
		if pos == -1 {
			return false
		}
		prevPos += pos + len(word)
	}
	return true
}

func allTrue(flags []bool) bool {
	for _, f := range flags {
		if !f {
			return false
		}
	}
	return true
}

// containsAllWords checks if text contains all the given words (in any order)
func containsAllWords(text string, words []string) bool {
	for _, word := range words {
		if !strings.Contains(text, word) {
			return false
		}
	}
	return true
}

// scoreBlock creates a blockRecord with match quality scores for the given block
func scoreBlock(block Block, searchPhrase string, searchWords []string, index int) blockRecord {
	lowerContent := strings.ToLower(block.Content)

	record := blockRecord{
		block:                       block,
		isDocument:                  block.IsDocument(),
		exactMatch:                  strings.Contains(lowerContent, searchPhrase),
		prefixMatch:                 strings.HasPrefix(lowerContent, searchPhrase),
		parentDocumentID:            block.DocumentID, // Set parentDocumentID for all blocks
		originalIndex:               index,
	}

	// NEW: Check if this block contains any of the search words
	for _, word := range searchWords {
		if strings.Contains(lowerContent, word) {
			record.containsAnySearchWord = true
			break
		}
	}

	if len(searchWords) > 1 {
		record.orderedWordsMatch = containsOrderedWords(lowerContent, searchWords)
		record.allWordsMatch = containsAllWords(lowerContent, searchWords)
	} else { // if only one search word, these are equivalent to exact match
		record.orderedWordsMatch = record.exactMatch
		record.allWordsMatch = record.exactMatch
	}

	// For document blocks, check if their title contains any search words
	if record.isDocument {
		lowerDocTitle := strings.ToLower(record.block.Content)
		for _, word := range searchWords {
			if strings.Contains(lowerDocTitle, word) {
				record.documentTitleRelevant = true
				break
			}
		}
	}

	if len(searchPhrase) > 0 {
		normTitle := normalizeAlnumLower(lowerContent)
		normQuery := normalizeAlnumLower(searchPhrase)

		if isSubsequence(normTitle, normQuery) {
			record.acronymMatch = true
		} else if isSubsequence(initials(block.Content), normQuery) {
			record.acronymMatch = true
		}
	}

	return record
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

func normalizeAlnumLower(s string) string {
	var b strings.Builder
	b.Grow(len(s))

	for _, r := range s {
		// Normalize to lowercase first
		if r >= 'A' && r <= 'Z' {
			r = r + ('a' - 'A')
		}

		// Keep only [a-z0-9]
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}

	return b.String()
}

func isSubsequence(text, pattern string) bool {
	if len(pattern) == 0 {
		return true
	}
	ti, pi := 0, 0
	for ti < len(text) && pi < len(pattern) {
		if text[ti] == pattern[pi] {
			pi++
		}
		ti++
	}
	return pi == len(pattern)
}

func initials(s string) string {
	parts := strings.Fields(s)
	var b strings.Builder
	b.Grow(len(parts))
	for _, p := range parts {
		if len(p) > 0 {
			b.WriteByte(byte(strings.ToLower(p)[0]))
		}
	}
	return b.String()
}

func (b *BlockRepo) searchWithLike(ctx context.Context, space Space, terms []string, limit int) (*sql.Rows, error) {
	tableNames := []string{"BlockSearch_content"}

	for _, tableName := range tableNames {
		var query string
		var args []interface{}

		if len(terms) == 0 {
			// No search terms, return recent documents only (not individual blocks)
			query = fmt.Sprintf(`
				SELECT c0 as id, c1 as content, c3 as entityType, c7 as documentId 
				FROM %s 
				WHERE c1 IS NOT NULL AND length(c1) > 0
				ORDER BY c0 DESC
				LIMIT ?
			`, tableName)
			args = []interface{}{limit}
		} else {
			conditions := make([]string, 0, len(terms)+1)
			args = make([]interface{}, 0, len(terms)+1)

			// Filter out empty content
			conditions = append(conditions, "c1 IS NOT NULL AND length(c1) > 0")

			// Use OR conditions for search terms to cast a wider net in SQL
			termConditions := make([]string, 0, len(terms))
			for _, term := range terms {
				lterm := strings.ToLower(term)
				termConditions = append(termConditions, "LOWER(c1) LIKE ?") // case-insensitive
				args = append(args, "%"+lterm+"%")
			}
			conditions = append(conditions, "("+strings.Join(termConditions, " OR ")+")")

			whereClause := strings.Join(conditions, " AND ") // Combine general conditions with the OR-group of terms

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

	log.Printf("All LIKE queries failed, trying basic search")
	return space.DB.QueryContext(ctx,
		"SELECT c0 as id, c1 as content, c3 as entityType, c7 as documentId FROM BlockSearch_content WHERE c1 IS NOT NULL AND length(c1) > 0 LIMIT ?",
		limit,
	)
}

func (b *BlockRepo) searchDocumentsWithLike(ctx context.Context, space Space, terms []string, limit int) (*sql.Rows, error) {
	tableNames := []string{"BlockSearch_content"}

	for _, tableName := range tableNames {
		var query string
		var args []interface{}

		if len(terms) == 0 {
			// Recent documents only
			query = fmt.Sprintf(`
				SELECT c0 as id, c1 as content, c3 as entityType, c7 as documentId
				FROM %s
				WHERE c3 = 'document' AND c1 IS NOT NULL AND length(c1) > 0
				ORDER BY c0 DESC
				LIMIT ?
			`, tableName)
			args = []interface{}{limit}
		} else {
			conditions := make([]string, 0, len(terms)+2)
			args = make([]interface{}, 0, len(terms)+2)

			conditions = append(conditions, "c3 = 'document'")
			conditions = append(conditions, "c1 IS NOT NULL AND length(c1) > 0")

			// Use OR conditions for search terms for document titles
			termConditions := make([]string, 0, len(terms))
			for _, term := range terms {
				lterm := strings.ToLower(term)
				termConditions = append(termConditions, "LOWER(c1) LIKE ?")
				args = append(args, "%"+lterm+"%")
			}
			conditions = append(conditions, "("+strings.Join(termConditions, " OR ")+")")

			whereClause := strings.Join(conditions, " AND ") // Combine general conditions with the OR-group of terms

			query = fmt.Sprintf(`
				SELECT c0 as id, c1 as content, c3 as entityType, c7 as documentId
				FROM %s
				WHERE %s
				ORDER BY c0 DESC
				LIMIT ?
			`, tableName, whereClause)
			args = append(args, limit)
		}

		log.Printf("Trying document-only LIKE query on %s: %s, args: %v", tableName, query, args)

		rows, err := space.DB.QueryContext(ctx, query, args...)
		if err == nil {
			return rows, nil
		}
		log.Printf("Document-only LIKE query on %s failed: %v", tableName, err)
	}

	log.Printf("All document-only LIKE queries failed, falling back to generic searchWithLike")
	return b.searchWithLike(ctx, space, terms, limit)
}

func (b *BlockRepo) Search(ctx context.Context, terms []string, allSpaces bool, daily bool, currentSpaceID string) ([]Block, error) {
	log.Printf("Searching with terms: %v", terms)

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

	var allBlocks []Block
	seenIDs := make(map[string]bool)

	// If no search terms, show recent documents (similar to Bear workflow)
	if len(terms) == 0 {
		log.Printf("No search terms, showing recent documents")
		for _, space := range spacesToSearch {
			rows, err := b.searchWithLike(ctx, space, []string{}, searchResultLimit)
			if err != nil {
				log.Printf("Recent documents query failed: %v", err)
				return nil, types.NewError("failed to query recent documents", err)
			}

			for rows.Next() {
				block := Block{SpaceID: space.ID}

				if err = rows.Scan(&block.ID, &block.Content, &block.EntityType, &block.DocumentID); err != nil {
					return nil, types.NewError("failed to scan a row", err)
				}

				if !seenIDs[block.ID] {
					allBlocks = append(allBlocks, block)
					seenIDs[block.ID] = true
				}
			}

			if err = rows.Err(); err != nil {
				return nil, types.NewError("error in rows", err)
			}

			if err = rows.Close(); err != nil {
				return nil, types.NewError("closing rows failed", err)
			}
		}

		return b.filterDateTitles(allBlocks, daily), nil
	}

	// Fuzzy search implementation similar to Bear workflow
	searchPhrase := strings.ToLower(strings.Join(terms, " "))
	searchWords := make([]string, len(terms))
	for i, term := range terms {
		searchWords[i] = strings.ToLower(term)
	}

	// Build quick lookup for spaces by ID
	spaceByID := make(map[string]Space, len(b.spaces))
	for _, s := range b.spaces {
		spaceByID[s.ID] = s
	}

	// First pass: search for full phrase
	if len(terms) > 0 {
		for _, space := range spacesToSearch {
			// 1) Documents first
			log.Printf("Searching %s for documents with full phrase, limit %d", space.ID, searchFetchLimit)

			docRows, err := b.searchDocumentsWithLike(ctx, space, terms, searchFetchLimit)
			if err != nil {
				log.Printf("Document LIKE search failed: %v", err)
				return nil, types.NewError("failed to query database search for documents", err)
			}

			for docRows.Next() {
				block := Block{SpaceID: space.ID}

				if err = docRows.Scan(&block.ID, &block.Content, &block.EntityType, &block.DocumentID); err != nil {
					return nil, types.NewError("failed to scan a document row", err)
				}

				if !seenIDs[block.ID] {
					allBlocks = append(allBlocks, block)
					seenIDs[block.ID] = true
				}
			}

			if err = docRows.Err(); err != nil {
				return nil, types.NewError("error in document rows", err)
			}

			if err = docRows.Close(); err != nil {
				return nil, types.NewError("closing document rows failed", err)
			}

			// 2) Then generic (docs + blocks)
			log.Printf("Searching %s for full phrase (all entities), limit %d", space.ID, searchFetchLimit)

			rows, err := b.searchWithLike(ctx, space, terms, searchFetchLimit)
			if err != nil {
				log.Printf("LIKE search failed: %v", err)
				return nil, types.NewError("failed to query database search", err)
			}

			for rows.Next() {
				block := Block{SpaceID: space.ID}

				if err = rows.Scan(&block.ID, &block.Content, &block.EntityType, &block.DocumentID); err != nil {
					return nil, types.NewError("failed to scan a row", err)
				}

				if !seenIDs[block.ID] {
					allBlocks = append(allBlocks, block)
					seenIDs[block.ID] = true
				}
			}

			if err = rows.Err(); err != nil {
				return nil, types.NewError("error in rows", err)
			}

			if err = rows.Close(); err != nil {
				return nil, types.NewError("closing rows failed", err)
			}
		}
	}

	// Score and rank all blocks
	records := make([]blockRecord, 0, len(allBlocks))
	docHasRealTitle := make(map[docKey]bool) // tracks docs already present as real document blocks

	// NEW: Declare highlyRelevantDocumentIDs here, before any 'if' blocks
	highlyRelevantDocumentIDs := make(map[string]bool)

	for i, block := range allBlocks {
		record := scoreBlock(block, searchPhrase, searchWords, i)
		records = append(records, record)

		if record.block.IsDocument() {
			docHasRealTitle[docKey{spaceID: block.SpaceID, docID: block.DocumentID}] = true
		}
	}

	// Build a map from documentID -> lowercased document title
	docTitles := make(map[string]string)
	for _, r := range records {
		if r.isDocument && r.block.DocumentID != "" {
			docTitles[r.block.DocumentID] = strings.ToLower(r.block.Content)
		}
	}

	// For each non-document block, see if (doc title + block content) together
	// contain all search words. If so, mark docAndBlockAllWordsMatch = true.
	for i := range records {
		if records[i].isDocument || records[i].parentDocumentID == "" {
			continue
		}

		docTitle, ok := docTitles[records[i].parentDocumentID]
		if !ok || len(searchWords) == 0 {
			continue
		}

		lowerBlock := strings.ToLower(records[i].block.Content)
		allFound := true
		for _, w := range searchWords {
			if !strings.Contains(lowerBlock, w) && !strings.Contains(docTitle, w) {
				allFound = false
				break
			}
		}

		if allFound {
			records[i].docAndBlockAllWordsMatch = true
		}
	}

	// Document-level aggregation: if all words appear somewhere in the document (across blocks),
	// ensure we have the REAL document title block in results.
	if len(searchWords) > 1 {
		// 1) Build per-document word hit map
		docWordHits := make(map[docKey][]bool)

		for _, block := range allBlocks {
			if block.DocumentID == "" {
				continue
			}
			k := docKey{spaceID: block.SpaceID, docID: block.DocumentID}

			hits, ok := docWordHits[k]
			if !ok {
				hits = make([]bool, len(searchWords))
			}

			lowerContent := strings.ToLower(block.Content)
			for i, w := range searchWords {
				if !hits[i] && strings.Contains(lowerContent, w) {
					hits[i] = true
				}
			}

			docWordHits[k] = hits
		}

		// 2) For each document where all search words are present across its blocks,
		// mark the document as highly relevant and, if needed, ensure we have the REAL
		// document row (c3='document') as a result.
		for k, hits := range docWordHits {
			if !allTrue(hits) {
				continue
			}

			// IMPORTANT:
			// Mark this document as highly relevant regardless of whether we already
			// have its real title block in results or not. This is what drives the
			// child-block boost later (parentDocumentWasTopRanked).
			highlyRelevantDocumentIDs[k.docID] = true

			// If we already have a real document block in results, nothing else to do.
			if docHasRealTitle[k] {
				continue
			}

			// Look up the space
			space, ok := spaceByID[k.spaceID]
			if !ok {
				continue
			}

			// Fetch the real document row from BlockSearch_content by documentId (c7)
			var (
				docID     string
				docTitle  string
				entityTyp string
				docDocID  string
			)

			row := space.DB.QueryRowContext(
				ctx,
				`SELECT c0 as id, c1 as content, c3 as entityType, c7 as documentId
				 FROM BlockSearch_content
				 WHERE c3 = 'document' AND c7 = ?
				 LIMIT 1`,
				k.docID,
			)

			if err := row.Scan(&docID, &docTitle, &entityTyp, &docDocID); err != nil {
				// If we can't load a real doc row, skip; don't create fake ones.
				continue
			}

			realDoc := Block{
				ID:         docID,
				SpaceID:    k.spaceID,
				Content:    docTitle,
				EntityType: entityTyp, // should be "document"
				DocumentID: docDocID,  // same as k.docID
			}

			// Mark that now we have the real document title
			docHasRealTitle[k] = true

			record := scoreBlock(realDoc, searchPhrase, searchWords, len(records))
			record.isDocument = true

			records = append(records, record)
		}
	}

	// The highlyRelevantDocumentIDs map is now populated only via document-level aggregation
	// where all search words appear across the document's blocks (see above).
	// This ensures it represents a stronger signal of overall document relevance.

	// Now, iterate through all records to mark child blocks of highly relevant documents
	for i := range records {
		// Only apply this to non-document blocks that actually have a parent document ID
		if !records[i].isDocument && records[i].parentDocumentID != "" {
			if highlyRelevantDocumentIDs[records[i].parentDocumentID] {
				records[i].parentDocumentWasTopRanked = true
			}
		}
	}

	// Sort by match strength first, with strong bias towards document title matches
	sort.SliceStable(records, func(i, j int) bool {
		iRecord := records[i]
		jRecord := records[j]

		// Priority 1: Strict Document Title Match
		// Documents whose title perfectly matches the entire search phrase, or contains all search words.
		iP1 := iRecord.isDocument && (iRecord.exactMatch || iRecord.orderedWordsMatch || iRecord.allWordsMatch)
		jP1 := jRecord.isDocument && (jRecord.exactMatch || jRecord.orderedWordsMatch || jRecord.allWordsMatch)
		if iP1 != jP1 {
			return iP1 // 'i' wins if it's a P1 match
		}

		// Priority 2: Parent Document where ALL search words were found across its blocks
		// (This is derived from highlyRelevantDocumentIDs.)
		iP2 := iRecord.isDocument && highlyRelevantDocumentIDs[iRecord.block.DocumentID]
		jP2 := jRecord.isDocument && highlyRelevantDocumentIDs[jRecord.block.DocumentID]
		if iP2 != jP2 {
			return iP2 // 'i' wins if it's a P2 match
		}

		// Priority 3: Child block where (document title + this block) together contain ALL search words
		// This is exactly the "Macrowhisper" (title) + "Release checklist" (block) scenario.
		iP3 := !iRecord.isDocument && iRecord.docAndBlockAllWordsMatch
		jP3 := !jRecord.isDocument && jRecord.docAndBlockAllWordsMatch
		if iP3 != jP3 {
			return iP3 // 'i' wins if it's a P3 match
		}

		// Priority 4: Relevant Child Block of a P2 Document
		iP4 := !iRecord.isDocument && iRecord.parentDocumentWasTopRanked && iRecord.containsAnySearchWord
		jP4 := !jRecord.isDocument && jRecord.parentDocumentWasTopRanked && jRecord.containsAnySearchWord
		if iP4 != jP4 {
			return iP4 // 'i' wins if it's a P4 match
		}

		// Priority 5: Document Title contains Any Search Word
		iP5 := iRecord.isDocument && iRecord.documentTitleRelevant
		jP5 := jRecord.isDocument && jRecord.documentTitleRelevant
		if iP5 != jP5 {
			return iP5 // 'i' wins if it's a P5 match
		}

		// --- General Match Strength (applies to any block/doc not covered by above specific boosts) ---

		// Priority 6: Prefix match
		if iRecord.prefixMatch != jRecord.prefixMatch {
			return iRecord.prefixMatch
		}

		// Priority 7: Exact phrase match
		if iRecord.exactMatch != jRecord.exactMatch {
			return iRecord.exactMatch
		}

		// Priority 8: Ordered words match
		if iRecord.orderedWordsMatch != jRecord.orderedWordsMatch {
			return iRecord.orderedWordsMatch
		}

		// Priority 9: All words match (any order)
		if iRecord.allWordsMatch != jRecord.allWordsMatch {
			return iRecord.allWordsMatch
		}

		// Priority 10: Acronym match
		if iRecord.acronymMatch != jRecord.acronymMatch {
			return iRecord.acronymMatch
		}
		// Tie-breaker for acronym matches: prefer documents
		if iRecord.acronymMatch && jRecord.acronymMatch {
			if iRecord.isDocument != jRecord.isDocument {
				return iRecord.isDocument
			}
		}

		// Priority 11: General preference for documents over non-documents
		if iRecord.isDocument != jRecord.isDocument {
			return iRecord.isDocument
		}

		// Priority 12: Blocks that contain at least one search word
		if iRecord.containsAnySearchWord != jRecord.containsAnySearchWord {
			return iRecord.containsAnySearchWord
		}

		// Final Tie-breaker: Maintain original search order for stability
		return iRecord.originalIndex < jRecord.originalIndex
	})

	// Convert back to blocks
	rankedBlocks := make([]Block, 0, len(records))
	for _, record := range records {
		rankedBlocks = append(rankedBlocks, record.block)
	}

	return b.filterDateTitles(rankedBlocks, daily), nil
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
