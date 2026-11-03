module github.com/UnderStudioHK/aish

go 1.26.0

require (
	github.com/chzyer/readline v1.5.1
	golang.org/x/term v0.47.0
)

require golang.org/x/sys v0.49.0 // indirect

replace github.com/chzyer/readline => ./third_party/readline
