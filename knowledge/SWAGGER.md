# Swagger API Documentation

This project includes Swagger/OpenAPI documentation for the Kubernetes Scheduler Extender API.

## Accessing Swagger UI

Once the service is running, you can access the Swagger UI at:

```
http://localhost:9000/swagger/index.html
```

## Available Endpoints

### Health Check
- **GET** `/health` - Returns the health status of the service

### Scheduler
- **POST** `/prioritize` - Prioritizes nodes for pod scheduling based on graph analysis

### Decisions
- **GET** `/decisions` - Lists all scheduling decisions for services in a namespace

## Regenerating Documentation

If you modify the API endpoints or their annotations, regenerate the Swagger docs:

```bash
# Ensure swag is installed
go install github.com/swaggo/swag/cmd/swag@latest

# Generate documentation
export PATH=$PATH:$(go env GOPATH)/bin
swag init -g cmd/best-node-selector/main.go --output ./docs
```

## Swagger Annotations

The API documentation is generated from code annotations:

- **General API info**: Located in `cmd/best-node-selector/main.go`
- **Endpoint details**: Located in `internal/transport/http/api.go`
- **Request/Response models**: Located in `internal/models/` (including `swagger_models.go` for Swagger-friendly representations)

### Swagger-Friendly Models

Since the actual `ExtenderArgs` type uses Kubernetes types (`v1.Pod`, `v1.Node`) which Swagger cannot parse, we've created simplified models in `internal/models/swagger_models.go`:

- `SwaggerExtenderArgs` - Simplified request body for `/prioritize` endpoint
- `SwaggerPod` - Simplified Kubernetes Pod representation
- `SwaggerNode` - Simplified Kubernetes Node representation

These models provide complete documentation in Swagger UI while the actual API still uses the standard Kubernetes types.

### Example Annotation Format

```go
// Health godoc
// @Summary Health check endpoint
// @Description Returns the health status of the service
// @Tags health
// @Accept json
// @Produce json
// @Success 200 {object} map[string]string
// @Router /health [get]
func (a *API) Health(w http.ResponseWriter, r *http.Request) {
    // handler implementation
}
```

## Configuration

The Swagger UI URL is configured in the router. To change the host/port, update:

```go
// internal/transport/http/router.go
httpSwagger.URL("http://localhost:9000/swagger/doc.json")
```

Make sure this matches your actual deployment host and port.

## Dependencies

- `github.com/swaggo/swag` - Swagger documentation generator
- `github.com/swaggo/http-swagger` - Swagger UI handler for standard Go HTTP
- `github.com/swaggo/files` - Static file serving for Swagger UI

These are automatically installed when you run:
```bash
go mod tidy
```
