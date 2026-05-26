# dpal

An MCP server providing access to DeepSeek models, including R1's separate reasoning channel.

Sibling of [gpal](https://github.com/tobert/gpal) (Gemini) and cpal (Claude). Implemented in Go.

## Status

Early scaffold. Single stateless tool — `consult_deepseek_oneshot`.

## Build

```sh
go build ./cmd/dpal
```

## Run

```sh
export DEEPSEEK_API_KEY=...
./dpal
```

Speaks MCP over stdio.
