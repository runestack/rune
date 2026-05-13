# Contributing to Rune

We love your input! We want to make contributing to Rune as easy and transparent as possible, whether it's:

- Reporting a bug
- Discussing the current state of the code
- Submitting a fix
- Proposing new features
- Becoming a maintainer

## Development Process

We use GitHub to host code, to track issues and feature requests, as well as accept pull requests.

1. Fork the repository
2. Create your feature branch (`git checkout -b feature/amazing-feature`)
3. Commit your changes (`git commit -m 'Add some amazing feature'`)
4. Push to the branch (`git push origin feature/amazing-feature`)
5. Open a Pull Request

## Pull Requests

1. Update the README.md with details of changes to the interface, if applicable.
2. Update examples and documentation.
3. The PR should work for all supported Go versions.
4. Ensure all tests pass.

## Coding Standards

### Go Code Style

- Follow standard Go conventions as outlined in [Effective Go](https://golang.org/doc/effective_go.html)
- Make sure your code is formatted with `gofmt`
- Follow the [Go Code Review Comments](https://github.com/golang/go/wiki/CodeReviewComments)
- Document all exported functions, types and constants
- Use meaningful variable names
- Keep functions small and focused

### Package Structure

- Use the standard Go project layout
- Place executables in `cmd/`
- Place libraries in `pkg/` (public) or `internal/` (private)
- User-facing documentation lives in the separate [runestack/docs](https://github.com/runestack/docs) repo (published at https://docs.runestack.io). Open PRs there for guides, references, and concepts
- Place tests beside the code they test

### Error Handling

- Always check errors
- Return errors rather than panic
- Use custom error types when appropriate
- Add context to errors when needed

### Testing

- Write unit tests for all functionality
- Write integration tests for complex interactions
- Aim for high test coverage
- Use table-driven tests when appropriate

### Documentation

- Document all exported functions, types, and constants
- Include examples in documentation when helpful
- Keep documentation up-to-date with code changes

## Git Commit Messages

- Use the present tense ("Add feature" not "Added feature")
- Use the imperative mood ("Move cursor to..." not "Moves cursor to...")
- Limit the first line to 72 characters or less
- Reference issues and pull requests after the first line

## License

By contributing, you agree that your contributions will be licensed under the project's MIT License.

## Code of Conduct

Please adhere to our [Code of Conduct](CODE_OF_CONDUCT.md) in all your interactions with the project. 