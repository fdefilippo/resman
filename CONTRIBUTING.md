# Contributing to ResMan

Thank you for your interest in contributing to Resource Manager! This document provides guidelines and instructions for contributing.

## Table of Contents

- [Code of Conduct](#code-of-conduct)
- [Getting Started](#getting-started)
- [Development Setup](#development-setup)
- [Making Changes](#making-changes)
- [Testing](#testing)
- [Submitting Changes](#submitting-changes)
- [Code Review](#code-review)
- [Release Process](#release-process)

---

## Code of Conduct

- Be respectful and inclusive
- Focus on constructive feedback
- Welcome newcomers and help them learn
- Keep discussions professional and on-topic

---

## Getting Started

### Where to Start

- Look for issues labeled [`good first issue`](https://github.com/fdefilippo/resman/issues?q=is%3Aissue+is%3Aopen+label%3A%22good+first+issue%22)
- Check issues labeled [`help wanted`](https://github.com/fdefilippo/resman/issues?q=is%3Aissue+is%3Aopen+label%3A%22help+wanted%22)
- Read existing documentation to understand the codebase

### Questions?

- Open an issue for questions
- Check existing issues and discussions
- Read the [README](README.md) and [documentation](docs/)

---

## Development Setup

### Prerequisites

- Go 1.21 or later
- Git
- Linux system with cgroups v2 support (for testing)
- Root access or appropriate capabilities (for integration tests)

### Fork and Clone

```bash
# Fork the repository on GitHub

# Clone your fork
git clone https://github.com/YOUR_USERNAME/resman.git
cd resman

# Add upstream remote
git remote add upstream https://github.com/fdefilippo/resman.git
```

### Install Dependencies

```bash
go mod download
go mod verify
```

### Build Locally

```bash
make build
```

### Run Tests

```bash
make test
```

---

## Making Changes

### Branch Naming

Use descriptive branch names:

```
feature/add-new-metric
fix/cgroup-permission-error
docs/update-readme
refactor/improve-error-handling
```

### Commit Messages

Follow [Conventional Commits](https://www.conventionalcommits.org/):

```
feat: add user memory metrics
fix: resolve cgroup permission issue
docs: update installation instructions
test: add unit tests for config package
refactor: improve error handling in collector
```

**Format:**
```
<type>(<scope>): <subject>

<body>

<footer>
```

**Types:**
- `feat`: New feature
- `fix`: Bug fix
- `docs`: Documentation
- `test`: Tests
- `refactor`: Code refactoring
- `chore`: Maintenance
- `perf`: Performance improvement

### Code Style

> **Read [`docs/DEVELOPMENT.md`](docs/DEVELOPMENT.md) before your first change.**
> It defines the architectural invariants a change to resman must respect
> (state model, configuration contracts, metric and error semantics, packaging)
> and ends with a Definition of Done checklist that reviewers apply to every
> pull request. This section covers only formatting and general hygiene.

- Follow Go best practices
- Run `go fmt` before committing
- Run `go vet` to catch issues
- Keep functions focused and small
- Add comments for complex logic
- Use meaningful variable names

### Adding Metrics

When adding new Prometheus metrics:

1. Add metric to `metrics/prometheus.go`
2. Register in `registerMetrics()`
3. Update through the typed `UpdateSystemSnapshot()` or `UpdateUserSnapshot()` contract
4. Update documentation in `docs/`
5. Add to Grafana dashboard
6. Update man page

---

## Testing

### Unit Tests

```bash
# Run all tests
go test -v ./...

# Run tests with coverage
go test -v -cover ./...

# Run specific package tests
go test -v ./config/...
go test -v ./metrics/...
```

### Integration Tests

```bash
# Requires root and cgroups v2
sudo make test-integration
```

### Fuzz Tests

Committed fuzz seeds run with the ordinary Go test suite. Generate additional inputs
locally with:

```bash
make fuzz                 # 30 seconds per target
make fuzz FUZZTIME=2m     # custom budget per target
```

Generated-input fuzzing is deliberately separate from the deterministic pull-request
gate. GitHub Actions runs `.github/workflows/fuzz.yml` every week with a two-minute
budget per target; maintainers can also start that workflow manually. A crasher fails
the workflow, which retains the command log, generated corpus, and Go fuzz cache as a
failure artifact.

### Manual Testing

1. Build the binary:
   ```bash
   go build -o resman
   ```

2. Create a test configuration:
   ```bash
   sudo install -d -m 0700 /etc/resman
   sudo install -m 0600 config/resman.conf.example /etc/resman/resman.conf
   ```

3. Run in debug mode:
   ```bash
   sudo ./resman --config /etc/resman/resman.conf --log-level DEBUG
   ```

4. Check metrics:
   ```bash
   curl http://localhost:1974/metrics
   ```

### Test Coverage

Aim for >80% coverage in new code:

```bash
# Generate coverage report
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out
```

---

## Submitting Changes

### Pull Request Process

1. **Create a branch**
   ```bash
   git checkout -b feature/your-feature
   ```

2. **Make changes**
   - Write code
   - Add tests
   - Update documentation
   - Update CHANGELOG.md

3. **Commit changes**
   ```bash
   git add .
   git commit -m "feat: add your feature"
   ```

4. **Push to your fork**
   ```bash
   git push origin feature/your-feature
   ```

5. **Open a Pull Request**
   - Use the PR template
   - Link related issues
   - Describe your changes clearly

### PR Checklist

Before submitting:

- [ ] Code is formatted (`go fmt`)
- [ ] Code passes linting (`go vet`)
- [ ] Tests pass (`make test`)
- [ ] Documentation updated
- [ ] CHANGELOG.md updated
- [ ] Commit messages follow conventions
- [ ] Branch is up to date with main

---

## Code Review

### Review Process

1. **Automated Checks**
   - CI/CD pipeline must pass
   - Tests must pass
   - Code coverage checks

2. **Maintainer Review**
   - At least one maintainer must approve
   - Address all review comments
   - Update PR as needed

3. **Merge**
   - Squash and merge for feature branches
   - Rebase and merge for simple fixes
   - Delete branch after merge

### Review Guidelines

**For reviewers:**
- Be constructive and respectful
- Explain reasoning for suggestions
- Approve when requirements are met
- Request changes for significant issues

**For contributors:**
- Respond to all comments
- Make requested changes or explain why not
- Thank reviewers for their time

---

## Release Process

### Version Numbering

Follow [Semantic Versioning](https://semver.org/):

- **MAJOR**: Breaking changes
- **MINOR**: New features (backward compatible)
- **PATCH**: Bug fixes (backward compatible)

### Release Checklist

For maintainers:

1. Run `make ci-quality` on `main`.
2. Update the release version in `Makefile`, `main.go`, the RPM spec, the Debian
   changelog, the man page, and current package/container examples. The version
   contract tests must remain green.
3. Create one release commit on `main`.
4. Create an annotated tag using the project convention:
   ```bash
   git tag -a v1.30.8 -m "Release 1.30.8"
   ```
5. Push `main` and the tag only when publication is intended.
6. GitHub Actions will:
   - Run all tests
   - Create packages (DEB, binary RPM, source RPM)
   - Create GitHub release

### Post-Release

- Update documentation
- Announce on social media
- Update package repositories
- Monitor for issues

---

## Documentation

### Updating Documentation

When adding features or fixing bugs:

1. **Code comments** - Explain complex logic
2. **README.md** - Update features list
3. **Man page** - Update `docs/resman.8`
4. **Prometheus docs** - Update `docs/prometheus-queries.md`
5. **Grafana** - Update `docs/dashboard-grafana-operations.json`

### Documentation Style

- Use clear, concise language
- Include examples
- Use markdown formatting
- Keep sections organized
- Add cross-references

---

## Getting Help

- **Issues**: Open an issue for bugs or questions
- **Discussions**: Use GitHub Discussions for general questions
- **Documentation**: Read the [docs/](docs/) directory
- **Code**: Read the source code with comments

---

## Recognition

Contributors will be:

- Listed in the README.md (optional)
- Mentioned in release notes
- Recognized in the community

Thank you for contributing to Resource Manager! 🎉
