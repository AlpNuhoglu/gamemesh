package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestResponses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/ok", func(c *gin.Context) { OK(c, gin.H{"hello": "world"}) })
	r.GET("/created", func(c *gin.Context) { Created(c, gin.H{"id": "1"}) })
	r.GET("/err", func(c *gin.Context) { Error(c, http.StatusTeapot, "nope") })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ok", nil))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"hello":"world"}`, w.Body.String())

	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/created", nil))
	assert.Equal(t, http.StatusCreated, w.Code)

	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/err", nil))
	assert.Equal(t, http.StatusTeapot, w.Code)
	assert.JSONEq(t, `{"error":"nope"}`, w.Body.String())
}
