package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/caarlos0/env/v6"
)

var regexIndexName = regexp.MustCompile(`^SearchIndex_([a-zA-Z0-9-]+(?:\|\|[a-zA-Z0-9-]+)*)\.sqlite$`)

// Primary space search index does not contain `||`, however, the search index
// for secondary spaces are named `primary||secondary`.

type SearchIndex struct {
	SpaceID string
	name    string
	dir     string
}

func (si SearchIndex) Path() string {
	return filepath.Join(si.dir, si.name)
}

type Config struct {
	IndexPathDir string `env:"INDEX_PATH_DIR" envDefault:"~/Library/Containers/com.lukilabs.lukiapp/Data/Library/Application Support/com.lukilabs.lukiapp/Search"`
	indexes      []SearchIndex
}

func (c *Config) SearchIndexes() []SearchIndex {
	return c.indexes
}

func (c *Config) MainDBPath() string {
	homeDir := os.Getenv("HOME")
	return filepath.Join(homeDir, "Library/Containers/com.lukilabs.lukiapp/Data/Library/Application Support/com.lukilabs.lukiapp/LukiMain_dbf93b0b-3c55-5ab0-745b-9fa6a60fc3d2_999609FB-390A-496E-9AA3-2F9B55D6C43C.realm")
}

func NewConfig() (*Config, error) {
	var config Config
	if err := env.Parse(&config); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}

	if strings.HasPrefix(config.IndexPathDir, "~/") {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("user home dir: %w", err)
		}

		config.IndexPathDir = strings.Replace(config.IndexPathDir, "~", homeDir, 1)
	}

	entries, err := os.ReadDir(config.IndexPathDir)
	if err != nil {
		return nil, fmt.Errorf("read dir: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		if regexIndexName.MatchString(entry.Name()) {
			match := regexIndexName.FindStringSubmatch(entry.Name())
			if len(match) < 2 {
				continue
			}
			spacePart := match[1]
			spaceIDs := strings.Split(spacePart, "||")
			config.indexes = append(config.indexes, SearchIndex{
				SpaceID: spaceIDs[len(spaceIDs)-1],
				name:    entry.Name(),
				dir:     config.IndexPathDir,
			})
		}
	}

	if len(config.indexes) == 0 {
		return nil, errors.New("no index files found")
	}

	return &config, nil
}
