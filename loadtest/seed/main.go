// Command seed prepares the data a load test needs and writes it as JSON for
// the k6 scripts: one seller with active listings, and a pool of buyers with
// Auth-emulator tokens (one per virtual user, because rate limits are
// per-user).
//
// It works through the same services the API uses, against the DATABASE_URL
// and FIREBASE_AUTH_EMULATOR_HOST in the environment, and refuses to run
// anywhere but development or test.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/dezmymachine/farmish-backend/internal/auth"
	"github.com/dezmymachine/farmish-backend/internal/config"
	"github.com/dezmymachine/farmish-backend/internal/database"
	"github.com/dezmymachine/farmish-backend/internal/db"
	"github.com/dezmymachine/farmish-backend/internal/sellers"
	"github.com/dezmymachine/farmish-backend/internal/users"
)

type fixtures struct {
	SellerID string    `json:"sellerId"`
	Listings []listing `json:"listings"`
	Buyers   []buyer   `json:"buyers"`
	Created  time.Time `json:"created"`
}

type listing struct {
	ID    string `json:"id"`
	Slug  string `json:"slug"`
	Title string `json:"title"`
}

type buyer struct {
	ID    string `json:"id"`
	Token string `json:"token"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "loadtest seed:", err)
		os.Exit(1)
	}
}

func run() error {
	out := flag.String("out", "loadtest/.tmp/fixtures.json", "where to write the fixtures JSON")
	buyers := flag.Int("buyers", 20, "buyer accounts to create (one per virtual user)")
	listings := flag.Int("listings", 30, "active listings to create")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.Env != config.EnvDevelopment && cfg.Env != config.EnvTest {
		return fmt.Errorf("refusing to seed load-test data into APP_ENV=%s", cfg.Env)
	}
	if cfg.Firebase.EmulatorHost == "" {
		return errors.New("FIREBASE_AUTH_EMULATOR_HOST is required: the load test signs in through the emulator")
	}

	ctx := context.Background()
	pool, err := database.Open(ctx, cfg.DB)
	if err != nil {
		return err
	}
	defer pool.Close()
	fb, err := auth.NewFirebase(ctx, cfg.Firebase)
	if err != nil {
		return err
	}
	usersSvc := users.New(pool)

	signIn := func(prefix string) (users.User, string, error) {
		token, err := emulatorSignUp(cfg.Firebase.EmulatorHost, prefix)
		if err != nil {
			return users.User{}, "", err
		}
		identity, err := fb.Verify(ctx, token)
		if err != nil {
			return users.User{}, "", fmt.Errorf("verify emulator token: %w", err)
		}
		u, err := usersSvc.Resolve(ctx, identity)
		return u, token, err
	}

	seller, _, err := signIn("load-seller")
	if err != nil {
		return err
	}
	if _, err := sellers.New(pool, nil, nil).UpsertMine(ctx, seller.ID, sellers.ProfileInput{
		BusinessName: "Load Test Farms", Region: "Ashanti", District: "Kumasi Metro",
	}); err != nil {
		return fmt.Errorf("seller profile: %w", err)
	}

	q := db.New(pool)
	categories, err := q.ListActiveCategories(ctx)
	if err != nil {
		return fmt.Errorf("list categories: %w (run `make seed` first)", err)
	}
	var categoryID uuid.UUID
	for _, c := range categories {
		if c.ParentID.Valid {
			categoryID = c.ID
			break
		}
	}
	if categoryID == uuid.Nil {
		return errors.New("no child category found: run the catalog seed first")
	}

	fx := &fixtures{SellerID: seller.ID.String(), Created: time.Now().UTC()}
	now := time.Now()
	expires := now.Add(30 * 24 * time.Hour)
	for i := range *listings {
		title := fmt.Sprintf("Load test maize lot %02d", i+1)
		slug := fmt.Sprintf("load-test-maize-%02d-%s", i+1, uuid.NewString()[:6])
		row, err := q.InsertListing(ctx, db.InsertListingParams{
			SellerID: seller.ID, CategoryID: categoryID, Title: title, Slug: slug,
			Description:  "Dry white maize for the load test, bagged and ready to collect.",
			PricePesewas: 25000 + int64(i)*100, Unit: "bags_50kg", QuantityAvailable: 1_000_000, MinOrderQty: 1,
			ItemState: "grade_a", Region: "Ashanti", District: "Kumasi Metro", OffersPickup: true,
		})
		if err != nil {
			return fmt.Errorf("insert listing: %w", err)
		}
		// A live listing always has an image (publishing requires one), and the
		// search and detail reads rely on it.
		media, err := q.InsertMediaObject(ctx, db.InsertMediaObjectParams{
			OwnerID: seller.ID, Key: "listings/" + row.ID.String() + "/" + uuid.NewString() + ".jpg",
			Purpose: "listing_image", ContentType: "image/jpeg", SizeBytes: 1024,
		})
		if err != nil {
			return fmt.Errorf("insert media: %w", err)
		}
		if _, err := q.AttachMediaObject(ctx, media.ID); err != nil {
			return fmt.Errorf("attach media: %w", err)
		}
		if err := q.InsertListingImage(ctx, db.InsertListingImageParams{ListingID: row.ID, MediaID: media.ID}); err != nil {
			return fmt.Errorf("attach listing image: %w", err)
		}
		if _, err := q.SetListingStatus(ctx, db.SetListingStatusParams{
			ID: row.ID, Status: "active", PublishedAt: &now, ExpiresAt: &expires,
		}); err != nil {
			return fmt.Errorf("publish listing: %w", err)
		}
		fx.Listings = append(fx.Listings, listing{ID: row.ID.String(), Slug: slug, Title: title})
	}

	for range *buyers {
		u, token, err := signIn("load-buyer")
		if err != nil {
			return err
		}
		fx.Buyers = append(fx.Buyers, buyer{ID: u.ID.String(), Token: token})
	}

	raw, err := json.MarshalIndent(fx, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, raw, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", *out, err)
	}
	fmt.Printf("loadtest seed: %d listings, %d buyers -> %s\n", len(fx.Listings), len(fx.Buyers), *out)
	return nil
}

// emulatorSignUp creates an email/password account in the Auth emulator and
// returns its ID token.
func emulatorSignUp(host, prefix string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"email":             fmt.Sprintf("%s-%s@farmish.test", prefix, uuid.NewString()[:8]),
		"password":          "load-test-" + uuid.NewString()[:8],
		"returnSecureToken": true,
	})
	if err != nil {
		return "", err
	}
	url := "http://" + strings.TrimPrefix(host, "http://") + "/identitytoolkit.googleapis.com/v1/accounts:signUp?key=emulator"
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("emulator sign-up: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var out struct {
		IDToken string `json:"idToken"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &out) != nil || out.IDToken == "" {
		return "", fmt.Errorf("emulator sign-up: status %d", resp.StatusCode)
	}
	return out.IDToken, nil
}
