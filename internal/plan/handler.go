package plan

import (
	"net/http"

	"github.com/labstack/echo/v4"
)

// Handler exposes discovered plans over HTTP.
type Handler struct {
	root string
}

// NewHandler constructs a plan handler rooted at root.
func NewHandler(root string) *Handler {
	return &Handler{root: root}
}

// HandleList returns all discovered plans.
func (h *Handler) HandleList(c echo.Context) error {
	plans, err := Discover(h.root)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, map[string]any{
		"plans":   plans,
		"count":   len(plans),
		"planDir": h.root,
	})
}

// HandleGet returns the full detail for one plan ID.
func (h *Handler) HandleGet(c echo.Context) error {
	detail, err := Load(h.root, c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, map[string]any{"plan": detail})
}
