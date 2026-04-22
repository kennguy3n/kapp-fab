package ktype

import (
	"encoding/json"
	"fmt"
)

// GenerateOpenAPISpec produces an OpenAPI 3.0 document covering the control
// plane (tenant lifecycle), the KType registry, and the CRUD operations for
// each registered KType. The spec is returned as JSON bytes so callers can
// serve it directly from an HTTP handler.
func GenerateOpenAPISpec(ktypes []KType) ([]byte, error) {
	paths := map[string]any{}
	paths["/api/v1/tenants"] = map[string]any{
		"post": operation("createTenant", "Create a tenant", refResponse("Tenant", 201)),
	}
	paths["/api/v1/tenants/{id}"] = map[string]any{
		"get": withPathParam(operation("getTenant", "Get a tenant", refResponse("Tenant", 200)), "id"),
	}
	paths["/api/v1/ktypes"] = map[string]any{
		"get":  operation("listKTypes", "List all KTypes", refResponseList("KType", 200)),
		"post": operation("registerKType", "Register a KType", refResponse("KType", 201)),
	}
	paths["/api/v1/ktypes/{name}"] = map[string]any{
		"get": withPathParam(operation("getKType", "Get a KType", refResponse("KType", 200)), "name"),
	}

	schemas := map[string]any{
		"Tenant":  tenantSchema(),
		"KType":   ktypeSchema(),
		"KRecord": krecordSchema(),
	}

	for _, kt := range ktypes {
		base := fmt.Sprintf("/api/v1/records/%s", kt.Name)
		schemas[fmt.Sprintf("%s_data", kt.Name)] = dataSchemaFromKType(kt)
		recordRef := fmt.Sprintf("%s_record", kt.Name)
		schemas[recordRef] = map[string]any{
			"allOf": []any{
				map[string]any{"$ref": "#/components/schemas/KRecord"},
				map[string]any{
					"type": "object",
					"properties": map[string]any{
						"data": map[string]any{"$ref": fmt.Sprintf("#/components/schemas/%s_data", kt.Name)},
					},
				},
			},
		}
		paths[base] = map[string]any{
			"get":  operation(fmt.Sprintf("list_%s", kt.Name), fmt.Sprintf("List %s records", kt.Name), refResponseList(recordRef, 200)),
			"post": operation(fmt.Sprintf("create_%s", kt.Name), fmt.Sprintf("Create a %s record", kt.Name), refResponse(recordRef, 201)),
		}
		paths[base+"/{id}"] = map[string]any{
			"get":    withPathParam(operation(fmt.Sprintf("get_%s", kt.Name), fmt.Sprintf("Get a %s record", kt.Name), refResponse(recordRef, 200)), "id"),
			"patch":  withPathParam(operation(fmt.Sprintf("update_%s", kt.Name), fmt.Sprintf("Update a %s record", kt.Name), refResponse(recordRef, 200)), "id"),
			"delete": withPathParam(operation(fmt.Sprintf("delete_%s", kt.Name), fmt.Sprintf("Delete a %s record", kt.Name), emptyResponse(204)), "id"),
		}
	}

	doc := map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title":       "Kapp Platform API",
			"version":     "0.1.0-phase-a",
			"description": "Generated OpenAPI spec for the Kapp tenant, KType and KRecord endpoints.",
		},
		"servers": []any{map[string]string{"url": "/"}},
		"paths":   paths,
		"components": map[string]any{
			"schemas": schemas,
			"parameters": map[string]any{
				"TenantHeader": map[string]any{
					"name":        "X-Tenant-ID",
					"in":          "header",
					"required":    true,
					"description": "Tenant UUID or slug.",
					"schema":      map[string]string{"type": "string"},
				},
			},
		},
	}
	return json.MarshalIndent(doc, "", "  ")
}

func operation(id, summary string, resp map[string]any) map[string]any {
	return map[string]any{
		"operationId": id,
		"summary":     summary,
		"responses":   resp,
	}
}

func refResponse(name string, status int) map[string]any {
	return map[string]any{
		fmt.Sprint(status): map[string]any{
			"description": "OK",
			"content": map[string]any{
				"application/json": map[string]any{
					"schema": map[string]string{"$ref": fmt.Sprintf("#/components/schemas/%s", name)},
				},
			},
		},
	}
}

func refResponseList(name string, status int) map[string]any {
	return map[string]any{
		fmt.Sprint(status): map[string]any{
			"description": "OK",
			"content": map[string]any{
				"application/json": map[string]any{
					"schema": map[string]any{
						"type":  "array",
						"items": map[string]string{"$ref": fmt.Sprintf("#/components/schemas/%s", name)},
					},
				},
			},
		},
	}
}

func emptyResponse(status int) map[string]any {
	return map[string]any{
		fmt.Sprint(status): map[string]any{"description": "No Content"},
	}
}

func withPathParam(op map[string]any, name string) map[string]any {
	op["parameters"] = []any{
		map[string]any{
			"name":     name,
			"in":       "path",
			"required": true,
			"schema":   map[string]string{"type": "string"},
		},
	}
	return op
}

func tenantSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id":     map[string]string{"type": "string", "format": "uuid"},
			"slug":   map[string]string{"type": "string"},
			"name":   map[string]string{"type": "string"},
			"status": map[string]string{"type": "string"},
			"plan":   map[string]string{"type": "string"},
		},
	}
}

func ktypeSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":    map[string]string{"type": "string"},
			"version": map[string]string{"type": "integer"},
			"schema":  map[string]string{"type": "object"},
		},
	}
}

func krecordSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id":        map[string]string{"type": "string", "format": "uuid"},
			"tenant_id": map[string]string{"type": "string", "format": "uuid"},
			"ktype":     map[string]string{"type": "string"},
			"version":   map[string]string{"type": "integer"},
			"status":    map[string]string{"type": "string"},
			"data":      map[string]string{"type": "object"},
		},
	}
}

func dataSchemaFromKType(kt KType) map[string]any {
	var parsed struct {
		Fields []FieldSpec `json:"fields"`
	}
	_ = json.Unmarshal(kt.Schema, &parsed)
	props := map[string]any{}
	var required []string
	for _, f := range parsed.Fields {
		props[f.Name] = openAPIType(f)
		if f.Required {
			required = append(required, f.Name)
		}
	}
	out := map[string]any{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

func openAPIType(f FieldSpec) map[string]any {
	switch f.Type {
	case "string", "text":
		s := map[string]any{"type": "string"}
		if f.MaxLength > 0 {
			s["maxLength"] = f.MaxLength
		}
		if f.Pattern != "" {
			s["pattern"] = f.Pattern
		}
		return s
	case "integer":
		return map[string]any{"type": "integer"}
	case "number", "float", "decimal":
		return map[string]any{"type": "number"}
	case "boolean":
		return map[string]any{"type": "boolean"}
	case "date":
		return map[string]any{"type": "string", "format": "date"}
	case "datetime":
		return map[string]any{"type": "string", "format": "date-time"}
	case "enum":
		return map[string]any{"type": "string", "enum": f.Values}
	case "ref":
		return map[string]any{"type": "string", "format": "uuid"}
	case "json":
		return map[string]any{"type": "object"}
	default:
		return map[string]any{"type": "string"}
	}
}
