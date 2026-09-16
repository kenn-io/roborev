package generated

//go:generate go run ../../../internal/daemon_client/openapi_generate -format yaml -o ../openapi.yaml
//go:generate go run github.com/doordash-oss/oapi-codegen-dd/v3/cmd/oapi-codegen@v3.75.14 -config config.yaml ../openapi.yaml
