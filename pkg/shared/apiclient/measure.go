package apiclient

import (
	"context"
	"net/url"
	"strconv"

	"github.com/trueforge-org/dexafix/pkg/models"
)

// ScalePage is one page of a user's scale readings, newest first.
type ScalePage struct {
	Readings []models.ScaleReading `json:"readings"`
	// Next is the cursor of the next (older) page; empty on the last page.
	Next string `json:"next,omitempty"`
}

func userPath(id int64, rest string) string { return "/users/" + strconv.FormatInt(id, 10) + rest }

// ScalePage returns the page of readings before the cursor (empty: the newest).
func (c *Client) ScalePage(ctx context.Context, userID int64, before string) (ScalePage, error) {
	var p ScalePage
	path := userPath(userID, "/pages/scale")
	if before != "" {
		path += "?before=" + url.QueryEscape(before)
	}
	err := c.Get(ctx, path, &p)
	return p, err
}

// CreateScaleReading stores a manual reading. The backend checks the values again.
func (c *Client) CreateScaleReading(ctx context.Context, userID int64, rd models.ScaleReading) error {
	return c.Post(ctx, userPath(userID, "/scale"), rd, nil)
}

func (c *Client) DeleteScaleReading(ctx context.Context, userID, id int64) error {
	return c.Delete(ctx, userPath(userID, "/scale/"+strconv.FormatInt(id, 10)))
}

// DexaScans returns all of a user's scans.
func (c *Client) DexaScans(ctx context.Context, userID int64) ([]models.DexaScan, error) {
	var scans []models.DexaScan
	err := c.Get(ctx, userPath(userID, "/pages/dexa"), &scans)
	return scans, err
}

func (c *Client) CreateDexaScan(ctx context.Context, userID int64, sc models.DexaScan) error {
	return c.Post(ctx, userPath(userID, "/dexa"), sc, nil)
}

func (c *Client) DeleteDexaScan(ctx context.Context, userID, id int64) error {
	return c.Delete(ctx, userPath(userID, "/dexa/"+strconv.FormatInt(id, 10)))
}
