# Agent Guidelines for Alfred CraftDocs Workflow

## Build & Test Commands
- **Build**: `go build ./app` - builds the main application
- **Test**: `go test ./...` - runs all tests (no test files currently exist)
- **Test single package**: `go test ./app/[package]` - test specific package
- **Run**: `go run ./app` - run the application directly

## Code Style Guidelines
- **Imports**: Standard library first, then external packages, then internal packages (separated by blank lines)
- **Error handling**: Use custom `types.Error` wrapper with `types.NewError(title, err)` for context
- **Naming**: PascalCase for exported types/functions, camelCase for unexported, descriptive names
- **Structs**: Use struct tags for environment variables (`env:"VAR_NAME" envDefault:"default"`)
- **Comments**: Use `//` for single-line comments, `/* */` for multi-line, add function documentation
- **SQL queries**: Use parameterized queries, close rows properly, handle errors with context
- **Logging**: Use `log.Printf()` for debugging search queries and operations
- **Context**: Always pass `context.Context` as first parameter to functions
- **Package structure**: Separate concerns - config, repository, service, types packages