# SmoothTalker

SmoothTalker is an intelligent LLM proxy designed to sit in front of an Ollama instance. Its primary purpose is to automatically route requests to the most appropriate model based on the content of the user's prompt, optimizing for a balance between capability (Cloud/Large models) and efficiency (Local/Small models).

## Features

- **Dynamic Model Routing**: When a request specifies `model: "recommended"`, SmoothTalker analyzes the prompt to determine if it requires high-reasoning capabilities (e.g., coding, architecture, debugging) or can be handled by a lightweight local model (e.g., greetings, simple facts).
- **Capability-Aware Selection**: The proxy checks for specific request requirements—such as image attachments (Vision) or tool definitions (Function Calling)—and ensures the selected model supports those capabilities.
- **Model Metadata Integration**: It leverages Ollama's `/api/tags` to understand the parameter size, quantization, and runner of available models to make informed routing decisions.
- **Automated Thinking Level**: Automatically injects a `thinking_level` option into requests to ensure optimal reasoning depth.
- **Harness Filtering**: Includes specialized logic to strip agent-specific harness markers (e.g., `<tools>`, `<rules>`, `<acp>` tags) from the prompt before sending it to the router, ensuring the routing decision is based on the actual user intent.
- **Simple Web Interface**: Includes a basic frontend for interacting with the proxy.

## How it Works

1. **Intercept**: The proxy listens on port `1337` and forwards traffic to an Ollama instance (default: `http://127.0.0.1:11434`).
2. **Analyze**: If the model is set to `"recommended"`, the proxy:
   - Fetches all available models from Ollama.
   - Identifies a "Router" model (one with the `decision` capability).
   - Extracts the core user intent from the prompt, stripping away metadata.
   - Sends the intent and a list of candidate models to the Router via a specialized `/v1/systemone` endpoint.
3. **Rewrite**: The proxy replaces `"recommended"` with the specific model name chosen by the router and adds necessary options.
4. **Forward**: The modified request is sent to the Ollama backend.

## Configuration

The following constants in `main.go` can be adjusted:
- `target`: The address of your Ollama server (default: `http://127.0.0.1:11434`).
- `listenAddr`: The port SmoothTalker listens on (default: `:1337`).
- `fallbackModel`: The model used if the router fails or no suitable model is found (default: `gemma4:e2b-mlx`).
  - *Note for Windows users*: Use `gemma4:e2b` instead of the `mlx` variant.

## Installation & Usage

### Prerequisites
- Go 1.21+
- A running Ollama instance with at least one model marked with the `decision` capability (for routing) and other candidate models.

### Running the Proxy
```bash
go run main.go
```

The server will start on `http://localhost:1337`.

### Example Request
Send a request to the proxy using `"recommended"` as the model:

```bash
curl http://localhost:1337/api/chat -d '{
  "model": "recommended",
  "messages": [
    { "role": "user", "content": "Write a Go function to calculate Fibonacci numbers" }
  ]
}'
```
SmoothTalker will route this to a high-capability (likely cloud/large) model.

## Project Structure
- `main.go`: The core proxy logic, routing engine, and request rewriter.
- `RouterSystemPrompt.md`: The instructions used by the router model to decide between LOCAL and CLOUD models.
- `web/`: A simple web interface for the proxy.
