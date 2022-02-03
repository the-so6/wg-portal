package ui

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/h44z/wg-portal/internal/authentication"
	"github.com/h44z/wg-portal/internal/core"

	"github.com/h44z/wg-portal/internal/persistence"

	"github.com/gin-gonic/gin"
	"github.com/h44z/wg-portal/cmd/wg-portal/common"
	"github.com/pkg/errors"
	csrf "github.com/utrack/gin-csrf"
)

type handler struct {
	config *common.Config

	session             SessionStore
	backend             core.Backend
	oauthAuthenticators map[string]authentication.Authenticator
	ldapAuthenticators  map[string]authentication.LdapAuthenticator
}

func NewHandler(config *common.Config, backend core.Backend) (*handler, error) {
	h := &handler{
		config:              config,
		backend:             backend,
		session:             GinSessionStore{sessionIdentifier: "wgPortalSession"},
		oauthAuthenticators: make(map[string]authentication.Authenticator),
		ldapAuthenticators:  make(map[string]authentication.LdapAuthenticator),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := h.setupAuthProviders(ctx)
	if err != nil {
		return nil, errors.WithMessage(err, "failed to setup authentication providers")
	}

	return h, nil
}

func (h *handler) setupAuthProviders(ctx context.Context) error {
	extUrl, err := url.Parse(h.config.Core.ExternalUrl)
	if err != nil {
		return errors.WithMessage(err, "failed to parse external url")
	}

	for i := range h.config.Auth.OpenIDConnect {
		providerCfg := &h.config.Auth.OpenIDConnect[i]
		providerId := strings.ToLower(providerCfg.ProviderName)

		if _, exists := h.oauthAuthenticators[providerId]; exists {
			return errors.Errorf("auth provider with name %s is already registerd", providerId)
		}

		redirectUrl := *extUrl
		redirectUrl.Path = path.Join(redirectUrl.Path, "/auth/login/", providerId, "/callback")

		authenticator, err := authentication.NewOidcAuthenticator(ctx, redirectUrl.String(), providerCfg)
		if err != nil {
			return errors.WithMessagef(err, "failed to setup oidc authentication provider %s", providerCfg.ProviderName)
		}
		h.oauthAuthenticators[providerId] = authenticator
	}
	for i := range h.config.Auth.OAuth {
		providerCfg := &h.config.Auth.OAuth[i]
		providerId := strings.ToLower(providerCfg.ProviderName)

		if _, exists := h.oauthAuthenticators[providerId]; exists {
			return errors.Errorf("auth provider with name %s is already registerd", providerId)
		}

		redirectUrl := *extUrl
		redirectUrl.Path = path.Join(redirectUrl.Path, "/auth/login/", providerId, "/callback")

		authenticator, err := authentication.NewPlainOauthAuthenticator(ctx, redirectUrl.String(), providerCfg)
		if err != nil {
			return errors.WithMessagef(err, "failed to setup oauth authentication provider %s", providerId)
		}
		h.oauthAuthenticators[providerId] = authenticator
	}
	for i := range h.config.Auth.Ldap {
		providerCfg := &h.config.Auth.Ldap[i]
		providerId := strings.ToLower(providerCfg.URL)

		if _, exists := h.ldapAuthenticators[providerId]; exists {
			return errors.Errorf("auth provider with name %s is already registerd", providerId)
		}

		authenticator, err := authentication.NewLdapAuthenticator(ctx, providerCfg)
		if err != nil {
			return errors.WithMessagef(err, "failed to setup ldap authentication provider %s", providerId)
		}
		h.ldapAuthenticators[providerId] = authenticator
	}

	return nil
}

func (h *handler) authenticationMiddleware(scope string) gin.HandlerFunc {
	return func(c *gin.Context) {
		session := h.session.GetData(c)

		if !session.LoggedIn {
			session.DeepLink = c.Request.RequestURI
			h.session.SetData(c, session)

			// Abort the request with the appropriate error code
			c.Abort()
			c.Redirect(http.StatusSeeOther, "/auth/login")
			return
		}

		if scope == "admin" && !session.IsAdmin {
			// Abort the request with the appropriate error code
			c.Abort()
			c.String(http.StatusUnauthorized, "unauthorized: not enough permissions")
			return
		}

		// default case if some random scope was set...
		if scope != "" && !session.IsAdmin {
			// Abort the request with the appropriate error code
			c.Abort()
			c.String(http.StatusUnauthorized, "unauthorized: not enough permissions")
			return
		}

		// Check if logged-in user is still valid
		if !h.isUserStillValid(session.UserIdentifier) {
			h.session.DestroyData(c)
			c.Abort()
			c.String(http.StatusUnauthorized, "unauthorized: session no longer available")
			return
		}

		// Continue down the chain to handler etc
		c.Next()
	}
}

func (h *handler) isUserStillValid(id persistence.UserIdentifier) bool {
	if _, err := h.backend.GetActiveUser(id); err != nil {
		return false
	}
	return true
}

func (h *handler) RegisterRoutes(g *gin.Engine) {
	csrfMiddleware := csrf.Middleware(csrf.Options{
		Secret: h.config.Core.SessionSecret,
		ErrorFunc: func(c *gin.Context) {
			c.String(400, "CSRF token mismatch")
			c.Abort()
		},
	})

	g.NoRoute(func(c *gin.Context) {
		h.HandleError(c, http.StatusNotFound,
			errors.New("Oops... you have reached the end of the internet"),
			fmt.Sprintf("Requested page %s not found", c.Request.URL.Path))
	})

	// Entrypoint
	g.GET("/", h.handleIndexGet())
	g.GET("/oops", h.handleErrorGet())

	// Auth routes
	auth := g.Group("/auth")
	auth.Use(csrfMiddleware)
	auth.GET("/login", h.handleLoginGet())
	auth.POST("/login", h.handleLoginPost())
	auth.GET("/login/:provider", h.handleLoginGetOauth())
	auth.GET("/login/:provider/callback", h.handleLoginGetOauthCallback())
	auth.GET("/logout", h.handleLogoutGet())

	// Admin routes
	admin := g.Group("/admin")
	admin.Use(csrfMiddleware)
	admin.Use(h.authenticationMiddleware("admin"))
	admin.GET("/", h.handleAdminIndexGet())
	admin.GET("/interface/new", h.handleAdminNewGet())
	admin.GET("/interface/create", h.handleAdminCreateGet())
	admin.GET("/interface/import", h.handleAdminImportGet())
	admin.GET("/users", h.handleAdminUserIndexGet())

	// User routes
}

func (h *handler) HandleError(c *gin.Context, code int, err error, details string) {
	currentSession := h.session.GetData(c)

	currentSession.Error = &ErrorData{
		Message: err.Error(),
		Details: details,
		Code:    code,
		Path:    c.Request.URL.Path,
	}

	// If page was not found, redirect to /
	if code == http.StatusNotFound {
		currentSession.Error.Path = "/"
	}

	h.session.SetData(c, currentSession)

	c.Redirect(http.StatusSeeOther, "/oops")
}

//
// --
//

type StaticData struct {
	WebsiteTitle string
	WebsiteLogo  string
	CompanyName  string
	Year         int
	Version      string
}