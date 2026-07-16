package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
)

type Client struct {
	clientID     string
	clientSecret string
	httpClient   *http.Client

	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time
}

func NewClient(clientID, clientSecret string) *Client {
	return &Client{
		clientID:     clientID,
		clientSecret: clientSecret,
		httpClient:   &http.Client{Timeout: 10 * time.Second},
	}
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

func (c *Client) getToken() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.accessToken != "" && time.Now().Before(c.expiresAt) {
		return c.accessToken, nil
	}

	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("scope", "https://api.ebay.com/oauth/api_scope")

	req, err := http.NewRequest(http.MethodPost, "https://api.ebay.com/identity/v1/oauth2/token",
		strings.NewReader(form.Encode()))

	if err != nil {
		return "", err
	}

	auth := base64.StdEncoding.EncodeToString([]byte(c.clientID + ":" + c.clientSecret))
	req.Header.Set("Authorization", "Basic "+auth)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("ebay oauth token request failed: %s: %s", resp.Status, body)
	}

	var tr tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", err
	}

	c.accessToken = tr.AccessToken
	c.expiresAt = time.Now().Add(time.Duration(tr.ExpiresIn-60) * time.Second)

	return c.accessToken, nil
}

// --- Caché de imágenes en memoria ---
// Guarda los bytes de la imagen y su content-type por un tiempo corto,
// así no le pedimos lo mismo a eBay en cada carga de cada usuario.
// Ojo: esto vive en RAM del servidor, se pierde si el servicio se reinicia
// (no reemplaza el caché en disco del celular, que ya lo hace CachedNetworkImage).

type cachedImage struct {
	data        []byte
	contentType string
	expiresAt   time.Time
}

type imageCache struct {
	mu    sync.Mutex
	items map[string]cachedImage
	ttl   time.Duration
}

func newImageCache(ttl time.Duration) *imageCache {
	return &imageCache{
		items: make(map[string]cachedImage),
		ttl:   ttl,
	}
}

func (c *imageCache) get(key string) (cachedImage, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	item, ok := c.items[key]
	if !ok || time.Now().After(item.expiresAt) {
		return cachedImage{}, false
	}
	return item, true
}

func (c *imageCache) set(key string, data []byte, contentType string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.items[key] = cachedImage{
		data:        data,
		contentType: contentType,
		expiresAt:   time.Now().Add(c.ttl),
	}
}

// allowedImageHost evita que el proxy se use para descargar cualquier URL
// (eso sería un "open proxy"). Solo dejamos pasar dominios de imágenes de eBay.
func allowedImageHost(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Host)
	return strings.HasSuffix(host, "ebayimg.com")
}

// ImageProxyHandler descarga la imagen de eBay del lado del servidor y la
// reenvía al cliente. Así, para el navegador, la imagen "viene" de nuestro
// propio dominio y no del dominio de eBay directamente.
func ImageProxyHandler(client *http.Client, cache *imageCache) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		imgURL := ctx.Query("url")
		if imgURL == "" {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": "falta el parámetro 'url'"})
			return
		}
		if !allowedImageHost(imgURL) {
			ctx.JSON(http.StatusForbidden, gin.H{"error": "host de imagen no permitido"})
			return
		}

		if item, ok := cache.get(imgURL); ok {
			ctx.Header("Cache-Control", "public, max-age=900")
			ctx.Data(http.StatusOK, item.contentType, item.data)
			return
		}

		req, err := http.NewRequest(http.MethodGet, imgURL, nil)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		resp, err := client.Do(req)
		if err != nil {
			ctx.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			ctx.JSON(resp.StatusCode, gin.H{"error": "eBay respondió con error al pedir la imagen"})
			return
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		contentType := resp.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "image/jpeg"
		}

		cache.set(imgURL, body, contentType)

		ctx.Header("Cache-Control", "public, max-age=900")
		ctx.Data(http.StatusOK, contentType, body)
	}
}

const browseSearchURL = "https://api.ebay.com/buy/browse/v1/item_summary/search"

func (c *Client) SearchBySeller(seller, sort string, limit, offset int) ([]byte, error) {
	token, err := c.getToken()
	if err != nil {
		return nil, err
	}

	q := url.Values{}
	q.Set("category_ids", "0")
	q.Set("filter", fmt.Sprintf("sellers:{%s},buyingOptions:{AUCTION|FIXED_PRICE}", seller))
	q.Set("limit", strconv.Itoa(limit))
	if offset > 0 {
		q.Set("offset", strconv.Itoa(offset))
	}
	if sort != "" {
		q.Set("sort", sort)
	}

	req, err := http.NewRequest(http.MethodGet, browseSearchURL+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-EBAY-C-MARKETPLACE-ID", "EBAY_US")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ebay browse search failed: %s: %s", resp.Status, body)
	}

	return body, nil
}

func ListingHandler(client *Client) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		seller := ctx.DefaultQuery("seller", os.Getenv("DEFAULT_SELLER_USERNAME"))
		sort := ctx.DefaultQuery("sort", "endTimeSoonest")

		// eBay Browse API acepta limit 1-200 y offset >= 0.
		limit, err := strconv.Atoi(ctx.DefaultQuery("limit", "50"))
		if err != nil || limit <= 0 || limit > 200 {
			limit = 50
		}
		offset, err := strconv.Atoi(ctx.DefaultQuery("offset", "0"))
		if err != nil || offset < 0 {
			offset = 0
		}

		data, err := client.SearchBySeller(seller, sort, limit, offset)
		if err != nil {
			ctx.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}

		ctx.Data(http.StatusOK, "application/json", data)
	}
}

func main() {
	godotenv.Load()

	client := NewClient(os.Getenv("EBAY_CLIENT_ID"), os.Getenv("EBAY_CLIENT_SECRET"))

	// CORS_ORIGINS: lista separada por comas (ej. "http://localhost:8080,https://mi-app.dev").
	// Si no se define, cae a localhost para que Flutter web funcione en pruebas locales.
	origins := os.Getenv("CORS_ORIGINS")
	allowOrigins := strings.Split(origins, ",")
	if origins == "" {
		allowOrigins = []string{"http://localhost:8080", "http://localhost:5000"}
	}

	router := gin.Default()
	router.Use(cors.New(cors.Config{
		AllowOrigins: allowOrigins,
	}))

	router.GET("/listings", ListingHandler(client))

	// Caché de imágenes en RAM por 15 minutos.
	imgCache := newImageCache(15 * time.Minute)
	imgHTTPClient := &http.Client{Timeout: 10 * time.Second}
	router.GET("/image-proxy", ImageProxyHandler(imgHTTPClient, imgCache))

	router.GET("/healtz", func(ctx *gin.Context) {
		ctx.JSON(http.StatusOK, gin.H{
			"status": "ok",
		})
	})

	router.GET("/helloworld", func(ctx *gin.Context) {
		ctx.JSON(http.StatusTeapot, gin.H{
			"hello": "teapot",
		})
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080" // valor por defecto para correrlo local sin .env
	}
	router.Run("0.0.0.0:" + port)
}