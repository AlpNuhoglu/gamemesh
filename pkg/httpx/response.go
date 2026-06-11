// Package httpx standardises JSON response envelopes across services so
// clients always parse the same shape.
package httpx

import "github.com/gin-gonic/gin"

// ErrorBody is the uniform error envelope.
type ErrorBody struct {
	Error string `json:"error"`
}

// OK writes a 200 response with the given payload.
func OK(c *gin.Context, data any) {
	c.JSON(200, data)
}

// Created writes a 201 response with the given payload.
func Created(c *gin.Context, data any) {
	c.JSON(201, data)
}

// Error writes a JSON error envelope and records the error on the gin
// context so logging middleware picks it up.
func Error(c *gin.Context, status int, msg string) {
	c.Error(errorString(msg)) //nolint:errcheck // gin collects these for logging
	c.AbortWithStatusJSON(status, ErrorBody{Error: msg})
}

type errorString string

func (e errorString) Error() string { return string(e) }
