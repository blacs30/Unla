package mcpproxy

import (
	"encoding/json"

	"github.com/amoylab/unla/pkg/mcp"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// convertMCPGoResult converts mcp-go result to local mcp format
func convertMCPGoResult(res *mcpgo.CallToolResult) *mcp.CallToolResult {
	result := &mcp.CallToolResult{
		IsError: res.IsError,
		Meta:    res.Meta,
	}

	// Process content items
	if len(res.Content) > 0 {
		var validContents []mcp.Content

		for _, content := range res.Content {
			// Skip null content
			if content == nil {
				continue
			}

			// Try to get content type
			contentType := ""
			switch c := content.(type) {
			case *mcpgo.TextContent:
				contentType = "text"
				validContents = append(validContents, &mcp.TextContent{
					Type: "text",
					Text: c.Text,
				})
			case *mcpgo.ImageContent:
				contentType = "image"
				validContents = append(validContents, &mcp.ImageContent{
					Type:     "image",
					Data:     c.Data,
					MimeType: c.MIMEType,
				})
			case *mcpgo.AudioContent:
				contentType = "audio"
				validContents = append(validContents, &mcp.AudioContent{
					Type:     "audio",
					Data:     c.Data,
					MimeType: c.MIMEType,
				})
			case *mcpgo.EmbeddedResource:
				contentType = "resource"
				switch resource := c.Resource.(type) {
				case mcpgo.TextResourceContents:
					validContents = append(validContents, &mcp.EmbeddedResource{
						Type: "resource",
						Resource: mcp.ResourceContents{
							URI:      resource.URI,
							MimeType: resource.MIMEType,
							Text:     resource.Text,
						},
					})
				case mcpgo.BlobResourceContents:
					validContents = append(validContents, &mcp.EmbeddedResource{
						Type: "resource",
						Resource: mcp.ResourceContents{
							URI:      resource.URI,
							MimeType: resource.MIMEType,
							Blob:     resource.Blob,
						},
					})
				}
			default:
				// Try to parse from raw content
				rawContent, err := json.Marshal(content)
				if err == nil {
					var contentMap map[string]interface{}
					if json.Unmarshal(rawContent, &contentMap) == nil {
						if typ, ok := contentMap["type"].(string); ok {
							contentType = typ

							switch contentType {
							case "text":
								if text, ok := contentMap["text"].(string); ok {
									validContents = append(validContents, &mcp.TextContent{
										Type: "text",
										Text: text,
									})
								}
							case "image":
								data, _ := contentMap["data"].(string)
								mimeType, _ := contentMap["mimeType"].(string)
								validContents = append(validContents, &mcp.ImageContent{
									Type:     "image",
									Data:     data,
									MimeType: mimeType,
								})
							case "audio":
								data, _ := contentMap["data"].(string)
								mimeType, _ := contentMap["mimeType"].(string)
								validContents = append(validContents, &mcp.AudioContent{
									Type:     "audio",
									Data:     data,
									MimeType: mimeType,
								})
							case "resource":
								if resource, ok := contentMap["resource"].(map[string]interface{}); ok {
									uri, _ := resource["uri"].(string)
									mimeType, _ := resource["mimeType"].(string)
									text, _ := resource["text"].(string)
									blob, _ := resource["blob"].(string)
									validContents = append(validContents, &mcp.EmbeddedResource{
										Type: "resource",
										Resource: mcp.ResourceContents{
											URI:      uri,
											MimeType: mimeType,
											Text:     text,
											Blob:     blob,
										},
									})
								}
							}
						}
					}
				}
			}
		}

		if len(validContents) > 0 {
			result.Content = validContents
		}
	}

	return result
}
