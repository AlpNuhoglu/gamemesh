package player

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alpnuhoglu/gamemesh/pkg/middleware"
)

func newHandlerRouter() (*gin.Engine, *Service) {
	svc, _, _ := newTestService()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewHandler(svc).RegisterRoutes(r)
	return r, svc
}

func doReq(r *gin.Engine, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	r.ServeHTTP(w, req)
	return w
}

func TestRegisterEndpoint(t *testing.T) {
	r, _ := newHandlerRouter()

	w := doReq(r, http.MethodPost, "/auth/register",
		`{"username":"alice","email":"alice@example.com","password":"password123"}`, nil)
	require.Equal(t, http.StatusCreated, w.Code)
	assert.Contains(t, w.Body.String(), `"username":"alice"`)
	assert.NotContains(t, w.Body.String(), "password", "hash must never be serialised")

	// Duplicate → 409.
	w = doReq(r, http.MethodPost, "/auth/register",
		`{"username":"alice","email":"alice@example.com","password":"password123"}`, nil)
	assert.Equal(t, http.StatusConflict, w.Code)
}

func TestRegisterValidation(t *testing.T) {
	r, _ := newHandlerRouter()
	cases := []string{
		`{"username":"al","email":"a@example.com","password":"password123"}`,   // too short
		`{"username":"alice","email":"not-an-email","password":"password123"}`, // bad email
		`{"username":"alice","email":"a@example.com","password":"short"}`,      // weak password
		`{"username":"has spaces","email":"a@example.com","password":"password123"}`,
		`not json`,
	}
	for _, body := range cases {
		w := doReq(r, http.MethodPost, "/auth/register", body, nil)
		assert.Equal(t, http.StatusBadRequest, w.Code, body)
	}
}

func TestLoginEndpoint(t *testing.T) {
	r, _ := newHandlerRouter()
	doReq(r, http.MethodPost, "/auth/register",
		`{"username":"alice","email":"alice@example.com","password":"password123"}`, nil)

	w := doReq(r, http.MethodPost, "/auth/login",
		`{"identifier":"alice","password":"password123"}`, nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"token"`)

	w = doReq(r, http.MethodPost, "/auth/login",
		`{"identifier":"alice","password":"wrong"}`, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestProfileEndpoints(t *testing.T) {
	r, svc := newHandlerRouter()
	w := doReq(r, http.MethodPost, "/auth/register",
		`{"username":"alice","email":"alice@example.com","password":"password123"}`, nil)
	require.Equal(t, http.StatusCreated, w.Code)

	p, err := svc.repo.GetByUsername(context.Background(), "alice")
	require.NoError(t, err)
	id := p.ID.String()

	// GET by id.
	w = doReq(r, http.MethodGet, "/players/"+id, "", nil)
	require.Equal(t, http.StatusOK, w.Code)

	// GET "me" resolves via the gateway-forwarded header.
	w = doReq(r, http.MethodGet, "/players/me", "", map[string]string{middleware.HeaderUserID: id})
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"alice"`)

	// Invalid UUID → 400.
	w = doReq(r, http.MethodGet, "/players/not-a-uuid", "", nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	// Update own profile.
	w = doReq(r, http.MethodPut, "/players/"+id, `{"username":"alicia"}`,
		map[string]string{middleware.HeaderUserID: id})
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"alicia"`)

	// Updating someone else's profile → 403.
	w = doReq(r, http.MethodPut, "/players/"+id, `{"username":"hacker"}`,
		map[string]string{middleware.HeaderUserID: "00000000-0000-0000-0000-000000000999"})
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestLogoutEndpoint(t *testing.T) {
	r, _ := newHandlerRouter()

	w := doReq(r, http.MethodPost, "/auth/logout", "", nil)
	assert.Equal(t, http.StatusBadRequest, w.Code, "missing JTI header")

	w = doReq(r, http.MethodPost, "/auth/logout", "", map[string]string{"X-Token-JTI": "some-jti"})
	assert.Equal(t, http.StatusOK, w.Code)
}
