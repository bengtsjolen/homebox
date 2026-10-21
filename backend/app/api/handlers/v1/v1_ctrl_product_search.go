package v1

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/hay-kot/httpkit/errchain"
	"github.com/hay-kot/httpkit/server"
	"github.com/rs/zerolog/log"
	"github.com/sysadminsmedia/homebox/backend/internal/data/repo"
	"github.com/sysadminsmedia/homebox/backend/internal/sys/config"
	"github.com/sysadminsmedia/homebox/backend/internal/web/adapters"
	"golang.org/x/text/unicode/norm"
)

const (
	barcodeHTTPTimeoutSec = 10
	schemeHTTPS           = "https"

	// barcodeMaxResponseBytes caps how much of an upstream response is decoded so a
	// misbehaving third-party API cannot drive unbounded allocation.
	barcodeMaxResponseBytes = 4 << 20 // 4 MiB
)

// barcodeHTTPClient is shared by every barcode lookup so outbound requests reuse a
// single connection pool instead of allocating a client per call.
var barcodeHTTPClient = &http.Client{Timeout: barcodeHTTPTimeoutSec * time.Second}

// decodeBarcodeResponse streams the response body into v. Decoding directly off the
// body avoids materializing the whole payload before parsing it.
func decodeBarcodeResponse(body io.Reader, v any) error {
	return json.NewDecoder(io.LimitReader(body, barcodeMaxResponseBytes)).Decode(v)
}

// flexibleString is a string that can be unmarshaled from either a JSON
// string or a JSON number. upcitemdb.com sometimes returns price fields
// (e.g. list_price, shipping) as numbers rather than strings, which would
// otherwise cause json.Unmarshal to fail on a plain string field.
type flexibleString string

// UnmarshalJSON accepts a JSON string, number, or null and stores the result
// as a Go string. Composite tokens (objects and arrays) are rejected so that
// unexpected shapes surface as clear errors instead of being silently stored
// as their raw representation.
func (f *flexibleString) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		*f = ""
		return nil
	}

	switch c := trimmed[0]; {
	case c == '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return err
		}
		*f = flexibleString(s)
		return nil
	case c == 'n':
		if string(trimmed) != "null" {
			return fmt.Errorf("flexibleString: unexpected token %q", string(trimmed))
		}
		*f = ""
		return nil
	case c == '-' || (c >= '0' && c <= '9'):
		// Validate the token is a well-formed JSON number, then store its literal form.
		var n json.Number
		if err := json.Unmarshal(trimmed, &n); err != nil {
			return err
		}
		*f = flexibleString(n.String())
		return nil
	default:
		return fmt.Errorf("flexibleString: cannot unmarshal %s into string", string(trimmed))
	}
}

type UPCITEMDBResponse struct {
	Code   string `json:"code"`
	Total  int    `json:"total"`
	Offset int    `json:"offset"`
	Items  []struct {
		Ean                  string   `json:"ean"`
		Title                string   `json:"title"`
		Description          string   `json:"description"`
		Upc                  string   `json:"upc"`
		Brand                string   `json:"brand"`
		Model                string   `json:"model"`
		Color                string   `json:"color"`
		Size                 string   `json:"size"`
		Dimension            string   `json:"dimension"`
		Weight               string   `json:"weight"`
		Category             string   `json:"category"`
		LowestRecordedPrice  float64  `json:"lowest_recorded_price"`
		HighestRecordedPrice float64  `json:"highest_recorded_price"`
		Images               []string `json:"images"`
		Offers               []struct {
			Merchant     string         `json:"merchant"`
			Domain       string         `json:"domain"`
			Title        string         `json:"title"`
			Currency     string         `json:"currency"`
			ListPrice    flexibleString `json:"list_price"`
			Price        float64        `json:"price"`
			Shipping     flexibleString `json:"shipping"`
			Condition    string         `json:"condition"`
			Availability string         `json:"availability"`
			Link         string         `json:"link"`
			UpdatedT     int            `json:"updated_t"`
		} `json:"offers"`
		Asin string `json:"asin"`
		Elid string `json:"elid"`
	} `json:"items"`
}

type OpenFactsResponse struct {
	Code    string           `json:"code"`
	Status  int              `json:"status"`
	Product openFactsProduct `json:"product"`
}

// Open Food Facts, Open Beauty Facts, and Open Products Facts share the same
// product response shape and API path, so one mapper can safely serve all three.
type openFactsProduct struct {
	ProductName   string `json:"product_name"`
	Brands        string `json:"brands"`
	Categories    string `json:"categories"`
	ImageFrontURL string `json:"image_front_url"`
	ImageURL      string `json:"image_url"`
	Quantity      string `json:"quantity"`
	GenericName   string `json:"generic_name"`
}

type openFactsSource struct {
	Name    string
	BaseURL string
}

var openFactsSources = []openFactsSource{
	{Name: "openfoodfacts.org", BaseURL: "https://world.openfoodfacts.org"},
	{Name: "openbeautyfacts.org", BaseURL: "https://world.openbeautyfacts.org"},
	{Name: "openproductsfacts.org", BaseURL: "https://world.openproductsfacts.org"},
}

type BARCODESPIDER_COMResponse struct {
	ItemResponse struct {
		Code    int    `json:"code"`
		Status  string `json:"status"`
		Message string `json:"message"`
	} `json:"item_response"`
	ItemAttributes struct {
		Title          string `json:"title"`
		Upc            string `json:"upc"`
		Ean            string `json:"ean"`
		ParentCategory string `json:"parent_category"`
		Category       string `json:"category"`
		Brand          string `json:"brand"`
		Model          string `json:"model"`
		Mpn            string `json:"mpn"`
		Manufacturer   string `json:"manufacturer"`
		Publisher      string `json:"publisher"`
		Asin           string `json:"asin"`
		Color          string `json:"color"`
		Size           string `json:"size"`
		Weight         string `json:"weight"`
		Image          string `json:"image"`
		IsAdult        string `json:"is_adult"`
		Description    string `json:"description"`
	} `json:"item_attributes"`
	Stores []struct {
		StoreName string `json:"store_name"`
		Title     string `json:"title"`
		Image     string `json:"image"`
		Price     string `json:"price"`
		Currency  string `json:"currency"`
		Link      string `json:"link"`
		Updated   string `json:"updated"`
	} `json:"Stores"`
}

func lookupUPCItemDB(iEan string) ([]repo.BarcodeProduct, error) {
	resp, err := barcodeHTTPClient.Get("https://api.upcitemdb.com/prod/trial/lookup?upc=" + url.QueryEscape(iEan))
	if err != nil {
		return nil, err
	}

	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API returned status code: %d", resp.StatusCode)
	}

	var result UPCITEMDBResponse
	if err := decodeBarcodeResponse(resp.Body, &result); err != nil {
		log.Error().Msg("Can not unmarshal JSON from upcitemdb.com")
		return nil, err
	}

	res := make([]repo.BarcodeProduct, 0, len(result.Items))

	for _, it := range result.Items {
		var p repo.BarcodeProduct
		p.SearchEngineName = "upcitemdb.com"
		p.Barcode = iEan

		p.Item.Description = it.Description
		p.Item.Name = it.Title
		p.Manufacturer = it.Brand
		p.ModelNumber = it.Model
		if len(it.Images) != 0 {
			p.ImageURL = it.Images[0]
		}

		res = append(res, p)
	}

	return res, nil
}

func lookupBarcodespider(tokenAPI string, iEan string) ([]repo.BarcodeProduct, error) {
	if len(tokenAPI) == 0 {
		return nil, errors.New("no api token configured for barcodespider. " +
			"Please define the api token in environment variable HBOX_BARCODE_TOKEN_BARCODESPIDER")
	}

	req, err := http.NewRequest(
		"GET", "https://api.barcodespider.com/v1/lookup?upc="+url.QueryEscape(iEan), nil)

	if err != nil {
		return nil, err
	}

	req.Header.Add("token", tokenAPI)

	resp, err := barcodeHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}

	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("barcodespider API returned status code: %d", resp.StatusCode)
	}

	var result BARCODESPIDER_COMResponse
	if err := decodeBarcodeResponse(resp.Body, &result); err != nil {
		log.Error().Msg("Can not unmarshal JSON from barcodespider.com")
		return nil, err
	}

	var p repo.BarcodeProduct
	p.Barcode = iEan
	p.SearchEngineName = "barcodespider.com"
	p.Item.Name = result.ItemAttributes.Title
	p.Item.Description = result.ItemAttributes.Description
	p.Manufacturer = result.ItemAttributes.Brand
	p.ModelNumber = result.ItemAttributes.Model
	p.ImageURL = result.ItemAttributes.Image

	return []repo.BarcodeProduct{p}, nil
}

// sanitizeHeader removes control characters that could cause HTTP header injection.
func sanitizeHeader(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7F {
			return -1
		}
		return r
	}, s)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

var openFactsAllowedImageDomains = []string{
	"openfoodfacts.org",
	"openbeautyfacts.org",
	"openproductsfacts.org",
}

func isAllowedOpenFactsImageHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))

	for _, domain := range openFactsAllowedImageDomains {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}

	return false
}

func normalizeOpenFactsImageURL(imageURL string) string {
	imageURL = strings.TrimSpace(imageURL)
	if imageURL == "" {
		return ""
	}

	u, err := url.Parse(imageURL)
	if err != nil || u.Hostname() == "" || u.User != nil {
		return ""
	}

	switch u.Scheme {
	case "http":
		u.Scheme = schemeHTTPS
	case schemeHTTPS:
	default:
		return ""
	}

	if !isAllowedOpenFactsImageHost(u.Hostname()) {
		return ""
	}

	return u.String()
}

func buildOpenFactsBarcodeProduct(sourceName string, iEan string, product openFactsProduct) (repo.BarcodeProduct, bool) {
	name := firstNonEmpty(product.ProductName, product.GenericName, product.Brands)
	if name == "" {
		return repo.BarcodeProduct{}, false
	}

	var p repo.BarcodeProduct
	p.Barcode = iEan
	p.SearchEngineName = sourceName
	p.Item.Name = name
	p.Manufacturer = product.Brands

	descriptionSources := [...]string{product.GenericName, product.Categories, product.Quantity}
	descriptionParts := make([]string, 0, len(descriptionSources))
	for _, value := range descriptionSources {
		value = strings.TrimSpace(value)
		if value != "" && value != name {
			descriptionParts = append(descriptionParts, value)
		}
	}
	p.Item.Description = strings.Join(descriptionParts, " | ")

	p.ImageURL = normalizeOpenFactsImageURL(firstNonEmpty(product.ImageFrontURL, product.ImageURL))

	return p, true
}

func lookupOpenFacts(contact string, source openFactsSource, iEan string) ([]repo.BarcodeProduct, error) {
	req, err := http.NewRequest(
		"GET", strings.TrimRight(source.BaseURL, "/")+"/api/v2/product/"+url.PathEscape(iEan)+".json", nil)
	if err != nil {
		return nil, err
	}
	userAgent := "Homebox/1.0 (https://github.com/sysadminsmedia/homebox)"
	safeContact := sanitizeHeader(strings.TrimSpace(contact))
	if len(safeContact) > 0 {
		userAgent = "Homebox/1.0 (contact: " + safeContact + ")"
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := barcodeHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}

	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s API returned status code: %d", source.Name, resp.StatusCode)
	}

	var result OpenFactsResponse
	if err := decodeBarcodeResponse(resp.Body, &result); err != nil {
		log.Error().Msg("Can not unmarshal " + source.Name + " JSON")
		return nil, err
	}

	if result.Status == 0 {
		return nil, nil
	}

	p, ok := buildOpenFactsBarcodeProduct(source.Name, iEan, result.Product)
	if !ok {
		return nil, nil
	}

	return []repo.BarcodeProduct{p}, nil
}

const openLibraryCoverHost = "covers.openlibrary.org"

type openLibraryBook struct {
	Title   string `json:"title"`
	Authors []struct {
		Name string `json:"name"`
	} `json:"authors"`
	Publishers []struct {
		Name string `json:"name"`
	} `json:"publishers"`
	PublishDate   string         `json:"publish_date"`
	NumberOfPages flexibleString `json:"number_of_pages"`
	Cover         struct {
		Small  string `json:"small"`
		Medium string `json:"medium"`
		Large  string `json:"large"`
	} `json:"cover"`
}

// compactBarcode removes spaces and dashes and uppercases a trailing ISBN-10 X.
func compactBarcode(raw string) string {
	var b strings.Builder
	b.Grow(len(raw))
	for _, r := range raw {
		if unicode.IsSpace(r) || unicode.Is(unicode.Dash, r) {
			continue
		}
		b.WriteRune(unicode.ToUpper(r))
	}
	return b.String()
}

func isbn13CheckDigit(digits12 string) byte {
	sum := 0
	for i := 0; i < 12; i++ {
		n := int(digits12[i] - '0')
		if i%2 == 0 {
			sum += n
		} else {
			sum += n * 3
		}
	}
	return byte('0' + (10-(sum%10))%10)
}

func validISBN10(code string) bool {
	if len(code) != 10 {
		return false
	}

	sum := 0
	for i := 0; i < 10; i++ {
		var value int
		switch c := code[i]; {
		case c >= '0' && c <= '9':
			value = int(c - '0')
		case i == 9 && c == 'X':
			value = 10
		default:
			return false
		}
		sum += value * (10 - i)
	}
	return sum%11 == 0
}

func validISBN13(code string) bool {
	if len(code) != 13 || (!strings.HasPrefix(code, "978") && !strings.HasPrefix(code, "979")) {
		return false
	}
	for i := 0; i < 13; i++ {
		if code[i] < '0' || code[i] > '9' {
			return false
		}
	}
	return code[12] == isbn13CheckDigit(code[:12])
}

// canonicalISBN13 reports the ISBN-13 for a compact ISBN-10 or ISBN-13.
// Other product codes are left for the retail barcode providers.
func canonicalISBN13(compact string) (string, bool) {
	switch {
	case validISBN13(compact):
		return compact, true
	case validISBN10(compact):
		body := "978" + compact[:9]
		return body + string(isbn13CheckDigit(body)), true
	default:
		return "", false
	}
}

func isAllowedOpenLibraryImageHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	return host == openLibraryCoverHost || strings.HasSuffix(host, "."+openLibraryCoverHost)
}

func normalizeOpenLibraryImageURL(imageURL string) string {
	imageURL = strings.TrimSpace(imageURL)
	if imageURL == "" {
		return ""
	}

	u, err := url.Parse(imageURL)
	if err != nil || u.Hostname() == "" || u.User != nil {
		return ""
	}

	switch u.Scheme {
	case "http":
		u.Scheme = schemeHTTPS
	case schemeHTTPS:
	default:
		return ""
	}

	if !isAllowedOpenLibraryImageHost(u.Hostname()) {
		return ""
	}

	return u.String()
}

func openLibraryCoverURL(isbn13 string, book openLibraryBook) string {
	candidate := normalizeOpenLibraryImageURL(firstNonEmpty(book.Cover.Large, book.Cover.Medium, book.Cover.Small))
	if candidate != "" {
		return candidate
	}
	return "https://" + openLibraryCoverHost + "/b/isbn/" + isbn13 + "-L.jpg?default=false"
}

func buildOpenLibraryBarcodeProduct(isbn13 string, book openLibraryBook) (repo.BarcodeProduct, bool) {
	title := strings.TrimSpace(book.Title)
	if title == "" {
		return repo.BarcodeProduct{}, false
	}

	authors := make([]string, 0, len(book.Authors))
	for _, author := range book.Authors {
		name := strings.TrimSpace(author.Name)
		if name != "" {
			authors = append(authors, name)
		}
	}

	publishers := make([]string, 0, len(book.Publishers))
	for _, publisher := range book.Publishers {
		name := strings.TrimSpace(publisher.Name)
		if name != "" {
			publishers = append(publishers, name)
		}
	}

	descriptionParts := make([]string, 0, 4)
	if len(publishers) > 0 {
		descriptionParts = append(descriptionParts, strings.Join(publishers, ", "))
	}
	if date := strings.TrimSpace(book.PublishDate); date != "" {
		descriptionParts = append(descriptionParts, date)
	}
	if pages, err := strconv.Atoi(string(book.NumberOfPages)); err == nil && pages > 0 {
		descriptionParts = append(descriptionParts, fmt.Sprintf("%d pages", pages))
	}
	descriptionParts = append(descriptionParts, "ISBN "+isbn13)

	var p repo.BarcodeProduct
	p.Barcode = isbn13
	p.SearchEngineName = "openlibrary.org"
	p.Item.Name = title
	p.Manufacturer = strings.Join(authors, ", ")
	p.Item.Description = strings.Join(descriptionParts, " | ")
	p.ImageURL = openLibraryCoverURL(isbn13, book)

	return p, true
}

func lookupOpenLibrary(contact string, isbn13 string) ([]repo.BarcodeProduct, error) {
	endpoint := "https://openlibrary.org/api/books?bibkeys=ISBN:" + url.QueryEscape(isbn13) + "&format=json&jscmd=data"
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}

	userAgent := "Homebox/1.0 (https://github.com/sysadminsmedia/homebox)"
	safeContact := sanitizeHeader(strings.TrimSpace(contact))
	if len(safeContact) > 0 {
		userAgent = "Homebox/1.0 (contact: " + safeContact + ")"
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := barcodeHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openlibrary.org API returned status code: %d", resp.StatusCode)
	}

	var result map[string]openLibraryBook
	if err := decodeBarcodeResponse(resp.Body, &result); err != nil {
		log.Error().Msg("Can not unmarshal openlibrary.org JSON")
		return nil, err
	}

	for _, book := range result {
		p, ok := buildOpenLibraryBarcodeProduct(isbn13, book)
		if ok {
			return []repo.BarcodeProduct{p}, nil
		}
	}

	return nil, nil
}

const (
	barcodeDescriptionLimit = 1000
	googleBooksImageHost    = "books.google.com"
)

type googleBooksVolume struct {
	ID         string `json:"id"`
	VolumeInfo struct {
		Title         string   `json:"title"`
		Subtitle      string   `json:"subtitle"`
		Authors       []string `json:"authors"`
		Publisher     string   `json:"publisher"`
		PublishedDate string   `json:"publishedDate"`
		Description   string   `json:"description"`
		PageCount     int      `json:"pageCount"`
		ImageLinks    struct {
			SmallThumbnail string `json:"smallThumbnail"`
			Thumbnail      string `json:"thumbnail"`
			Small          string `json:"small"`
			Medium         string `json:"medium"`
			Large          string `json:"large"`
			ExtraLarge     string `json:"extraLarge"`
		} `json:"imageLinks"`
		IndustryIdentifiers []struct {
			Type       string `json:"type"`
			Identifier string `json:"identifier"`
		} `json:"industryIdentifiers"`
	} `json:"volumeInfo"`
	SaleInfo struct {
		IsEbook bool `json:"isEbook"`
	} `json:"saleInfo"`
}

type googleBooksResponse struct {
	Items []googleBooksVolume `json:"items"`
}

func stripHTML(value string) string {
	var b strings.Builder
	inTag := false
	for _, r := range value {
		switch {
		case r == '<':
			inTag = true
			b.WriteByte(' ')
		case r == '>':
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func limitRunes(text string, limit int) (string, bool) {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) <= limit {
		return string(runes), false
	}
	if limit < 1 {
		return "", true
	}
	return strings.TrimSpace(string(runes[:limit])), true
}

// withISBN keeps the ISBN on the description and stays inside the item field limit.
func withISBN(body string, isbn13 string) string {
	note := "ISBN " + isbn13
	body = strings.TrimSpace(stripHTML(body))
	if body == "" {
		return note
	}
	if strings.Contains(body, isbn13) {
		text, cut := limitRunes(body, barcodeDescriptionLimit)
		if cut && len([]rune(text)) > 0 {
			text, _ = limitRunes(text, barcodeDescriptionLimit-1)
			return text + "…"
		}
		return text
	}

	suffix := " | " + note
	room := barcodeDescriptionLimit - len([]rune(suffix))
	text, cut := limitRunes(body, room)
	if cut && room > 1 {
		text, _ = limitRunes(body, room-1)
		text += "…"
	}
	if text == "" {
		return note
	}
	return text + suffix
}

func isAllowedGoogleBooksImageHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	return host == googleBooksImageHost || host == "books.googleusercontent.com" || strings.HasSuffix(host, ".googleusercontent.com")
}

func normalizeGoogleBooksImageURL(imageURL string) string {
	imageURL = strings.TrimSpace(imageURL)
	if imageURL == "" {
		return ""
	}

	u, err := url.Parse(imageURL)
	if err != nil || u.Hostname() == "" || u.User != nil {
		return ""
	}

	switch u.Scheme {
	case "http":
		u.Scheme = schemeHTTPS
	case schemeHTTPS:
	default:
		return ""
	}

	if !isAllowedGoogleBooksImageHost(u.Hostname()) {
		return ""
	}

	return u.String()
}

func googleBooksCoverURL(volume googleBooksVolume) string {
	links := volume.VolumeInfo.ImageLinks
	return normalizeGoogleBooksImageURL(firstNonEmpty(
		links.ExtraLarge,
		links.Large,
		links.Medium,
		links.Small,
		links.Thumbnail,
		links.SmallThumbnail,
	))
}

func googleBooksCoverURLOrVolume(volume googleBooksVolume) string {
	if cover := googleBooksCoverURL(volume); cover != "" {
		return cover
	}
	// The Books site draws a cover from the volume id even when imageLinks is empty.
	if id := strings.TrimSpace(volume.ID); id != "" {
		return "https://books.google.com/books/content?id=" + url.QueryEscape(id) + "&printsec=frontcover&img=1&zoom=1"
	}
	return ""
}

func buildGoogleBooksBarcodeProduct(isbn13 string, volume googleBooksVolume) (repo.BarcodeProduct, bool) {
	info := volume.VolumeInfo
	title := strings.TrimSpace(info.Title)
	if title == "" {
		return repo.BarcodeProduct{}, false
	}
	subtitle := strings.TrimSpace(info.Subtitle)
	if subtitle != "" && !strings.Contains(strings.ToLower(title), strings.ToLower(subtitle)) {
		title += ": " + subtitle
	}

	authors := make([]string, 0, len(info.Authors))
	for _, author := range info.Authors {
		author = strings.TrimSpace(author)
		if author != "" {
			authors = append(authors, author)
		}
	}

	var body string
	if description := strings.TrimSpace(info.Description); description != "" {
		body = description
	} else {
		parts := make([]string, 0, 3)
		if publisher := strings.TrimSpace(info.Publisher); publisher != "" {
			parts = append(parts, publisher)
		}
		if date := strings.TrimSpace(info.PublishedDate); date != "" {
			parts = append(parts, date)
		}
		if info.PageCount > 0 {
			parts = append(parts, fmt.Sprintf("%d pages", info.PageCount))
		}
		body = strings.Join(parts, " | ")
	}

	var p repo.BarcodeProduct
	p.Barcode = isbn13
	p.SearchEngineName = googleBooksImageHost
	p.Item.Name = title
	p.Manufacturer = strings.Join(authors, ", ")
	p.Item.Description = withISBN(body, isbn13)
	p.ImageURL = googleBooksCoverURL(volume)

	return p, true
}

func selectGoogleBooksVolume(items []googleBooksVolume, isbn13 string) (googleBooksVolume, bool) {
	var fallback *googleBooksVolume
	for i := range items {
		if strings.TrimSpace(items[i].VolumeInfo.Title) == "" {
			continue
		}
		if fallback == nil {
			fallback = &items[i]
		}
		for _, id := range items[i].VolumeInfo.IndustryIdentifiers {
			if compactBarcode(id.Identifier) == isbn13 {
				return items[i], true
			}
		}
	}
	if fallback == nil {
		return googleBooksVolume{}, false
	}
	return *fallback, true
}

func bookTitleKey(title string, subtitle string) string {
	full := strings.TrimSpace(title)
	if subtitle = strings.TrimSpace(subtitle); subtitle != "" && !strings.Contains(strings.ToLower(full), strings.ToLower(subtitle)) {
		full += " " + subtitle
	}

	full = foldLetters(full)
	var b strings.Builder
	for _, r := range full {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func authorsOverlap(left []string, right []string) bool {
	if len(left) == 0 || len(right) == 0 {
		return len(left) == 0 && len(right) == 0
	}

	tokens := make(map[string]struct{})
	for _, name := range left {
		for _, part := range strings.Fields(foldLetters(name)) {
			part = strings.Trim(part, ",.;")
			if len([]rune(part)) >= 3 {
				tokens[part] = struct{}{}
			}
		}
	}
	for _, name := range right {
		for _, part := range strings.Fields(foldLetters(name)) {
			part = strings.Trim(part, ",.;")
			if _, ok := tokens[part]; ok {
				return true
			}
		}
	}
	return false
}

func foldLetters(value string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(strings.ToLower(value)) {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func sameEditionTitle(source googleBooksVolume, candidate googleBooksVolume) bool {
	left := bookTitleKey(source.VolumeInfo.Title, source.VolumeInfo.Subtitle)
	right := bookTitleKey(candidate.VolumeInfo.Title, candidate.VolumeInfo.Subtitle)
	return left != "" && left == right
}

func googleVolumeHasISBN(volume googleBooksVolume, isbn13 string) bool {
	for _, id := range volume.VolumeInfo.IndustryIdentifiers {
		if compactBarcode(id.Identifier) == isbn13 {
			return true
		}
	}
	return false
}

// googleBooksSiblingCover returns a cover from another edition of the same book.
// The exact ISBN often has no image while another printing from the same year does.
func googleBooksSiblingCover(isbn13 string, source googleBooksVolume, candidates []googleBooksVolume) string {
	want := bookTitleKey(source.VolumeInfo.Title, source.VolumeInfo.Subtitle)
	if want == "" {
		return ""
	}

	year := ""
	if len(source.VolumeInfo.PublishedDate) >= 4 {
		year = source.VolumeInfo.PublishedDate[:4]
	}

	bestCover := ""
	bestScore := -1
	for _, candidate := range candidates {
		if year == "" || !strings.HasPrefix(candidate.VolumeInfo.PublishedDate, year) {
			continue
		}
		if googleVolumeHasISBN(candidate, isbn13) || !sameEditionTitle(source, candidate) {
			continue
		}
		if !authorsOverlap(source.VolumeInfo.Authors, candidate.VolumeInfo.Authors) {
			continue
		}
		cover := googleBooksCoverURLOrVolume(candidate)
		if cover == "" {
			continue
		}
		// A printed edition outranks an ebook. Page count is not compared.
		score := 1
		if !candidate.SaleInfo.IsEbook {
			score += 2
		}
		if score > bestScore {
			bestCover = cover
			bestScore = score
		}
	}
	return bestCover
}

func shortBookTitle(title string) string {
	title = strings.TrimSpace(title)
	if head, _, ok := strings.Cut(title, ":"); ok && strings.TrimSpace(head) != "" {
		return strings.TrimSpace(head)
	}
	return title
}

func editionSubtitle(volume googleBooksVolume) string {
	if subtitle := strings.TrimSpace(volume.VolumeInfo.Subtitle); subtitle != "" {
		return subtitle
	}
	if _, rest, ok := strings.Cut(volume.VolumeInfo.Title, ":"); ok {
		return strings.TrimSpace(rest)
	}
	return ""
}

func sourceLabel(engine string) string {
	switch engine {
	case googleBooksImageHost:
		return "Google Books"
	case "openlibrary.org":
		return "Open Library"
	case dnbSearchEngineName:
		return "Deutsche Nationalbibliothek"
	default:
		return engine
	}
}

func googleQueryTerm(prefix string, value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, `"`, ""))
	if value == "" {
		return ""
	}
	if strings.ContainsAny(value, " \t") {
		return prefix + `"` + value + `"`
	}
	return prefix + value
}

func fetchGoogleBooksVolumes(apiKey string, queryText string, maxResults int) ([]googleBooksVolume, error) {
	query := url.Values{}
	query.Set("q", queryText)
	query.Set("printType", "books")
	query.Set("maxResults", strconv.Itoa(maxResults))
	if strings.TrimSpace(apiKey) != "" {
		query.Set("key", strings.TrimSpace(apiKey))
	}

	req, err := http.NewRequest(http.MethodGet, "https://www.googleapis.com/books/v1/volumes?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Homebox/1.0 (https://github.com/sysadminsmedia/homebox)")

	resp, err := barcodeHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	switch resp.StatusCode {
	case http.StatusNotFound:
		return nil, nil
	case http.StatusTooManyRequests, http.StatusForbidden:
		log.Warn().Msg("google books lookup unavailable (set HBOX_BARCODE_TOKEN_GOOGLE_BOOKS): " + resp.Status)
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("books.google.com API returned status code: %d", resp.StatusCode)
	}

	var result googleBooksResponse
	if err := decodeBarcodeResponse(resp.Body, &result); err != nil {
		log.Error().Msg("Can not unmarshal books.google.com JSON")
		return nil, err
	}
	return result.Items, nil
}

func lookupGoogleBooks(apiKey string, isbn13 string) ([]repo.BarcodeProduct, error) {
	items, err := fetchGoogleBooksVolumes(apiKey, "isbn:"+isbn13, 5)
	if err != nil || len(items) == 0 {
		return nil, err
	}

	volume, ok := selectGoogleBooksVolume(items, isbn13)
	if !ok {
		return nil, nil
	}
	product, ok := buildGoogleBooksBarcodeProduct(isbn13, volume)
	if !ok {
		return nil, nil
	}
	if product.ImageURL == "" {
		product.ImageURL = googleBooksSiblingCover(isbn13, volume, items)
	}
	if product.ImageURL == "" && strings.TrimSpace(apiKey) != "" {
		// Ask for the same title and subtitle. Google prints those on two lines.
		query := googleQueryTerm("intitle:", shortBookTitle(volume.VolumeInfo.Title))
		if subtitle := editionSubtitle(volume); subtitle != "" {
			query = strings.TrimSpace(query + " " + googleQueryTerm("intitle:", subtitle))
		}
		if len(volume.VolumeInfo.Authors) > 0 {
			if author := googleQueryTerm("inauthor:", volume.VolumeInfo.Authors[0]); author != "" {
				query = strings.TrimSpace(query + " " + author)
			}
		}
		related, err := fetchGoogleBooksVolumes(apiKey, query, 10)
		if err != nil {
			log.Warn().Msg("google books edition search failed: " + err.Error())
		} else if cover := googleBooksSiblingCover(isbn13, volume, related); cover != "" {
			product.ImageURL = cover
		}
	}
	if product.ImageURL == "" {
		product.ImageURL = googleBooksCoverURLOrVolume(volume)
	}
	return []repo.BarcodeProduct{product}, nil
}

const dnbSearchEngineName = "www.dnb.de"

var (
	catalogRolePattern = regexp.MustCompile(`\s*\[[^\]]*\]`)
	catalogPagePattern = regexp.MustCompile(`(\d+)\s+pages`)
)

type dnbRecord struct {
	titles     []string
	creators   []string
	publishers []string
	dates      []string
	formats    []string
}

// parseDNBSRU reads a Deutsche Nationalbibliothek SRU response in oai_dc.
func parseDNBSRU(data []byte) (dnbRecord, int, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	var record dnbRecord
	var records int
	var current string
	var capture bool
	var buf strings.Builder

	flush := func() {
		text := strings.TrimSpace(buf.String())
		buf.Reset()
		if text == "" || !capture {
			return
		}
		switch current {
		case "title":
			record.titles = append(record.titles, text)
		case "creator":
			record.creators = append(record.creators, text)
		case "publisher":
			record.publishers = append(record.publishers, text)
		case "date":
			record.dates = append(record.dates, text)
		case "format":
			record.formats = append(record.formats, text)
		case "numberOfRecords":
			parsed, err := strconv.Atoi(text)
			if err == nil {
				records = parsed
			}
		}
	}

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			flush()
			return record, records, nil
		}
		if err != nil {
			return record, records, err
		}
		switch el := tok.(type) {
		case xml.StartElement:
			flush()
			current = el.Name.Local
			switch current {
			case "title", "creator", "publisher", "date", "format", "numberOfRecords":
				capture = true
			default:
				capture = false
			}
		case xml.CharData:
			if capture {
				buf.Write(el)
			}
		case xml.EndElement:
			flush()
			current = ""
			capture = false
		}
	}
}

func catalogPersonName(raw string) string {
	raw = strings.TrimSpace(catalogRolePattern.ReplaceAllString(raw, ""))
	last, first, ok := strings.Cut(raw, ",")
	if !ok {
		return raw
	}
	last = strings.TrimSpace(last)
	first = strings.TrimSpace(first)
	if last == "" || first == "" {
		return raw
	}
	return first + " " + last
}

func catalogTitle(raw string) string {
	raw = strings.TrimSpace(raw)
	if statement, _, ok := strings.Cut(raw, " / "); ok && strings.TrimSpace(statement) != "" {
		raw = strings.TrimSpace(statement)
	}
	return strings.ReplaceAll(raw, " : ", ": ")
}

func buildDNBBarcodeProduct(isbn13 string, record dnbRecord) (repo.BarcodeProduct, bool) {
	if len(record.titles) == 0 {
		return repo.BarcodeProduct{}, false
	}
	title := catalogTitle(record.titles[0])
	if title == "" {
		return repo.BarcodeProduct{}, false
	}

	authors := make([]string, 0, len(record.creators))
	for _, creator := range record.creators {
		name := catalogPersonName(creator)
		if name != "" {
			authors = append(authors, name)
		}
	}

	parts := make([]string, 0, 3)
	if len(record.publishers) > 0 {
		if publisher := strings.TrimSpace(record.publishers[0]); publisher != "" {
			parts = append(parts, publisher)
		}
	}
	if len(record.dates) > 0 {
		if date := strings.TrimSpace(record.dates[0]); date != "" {
			parts = append(parts, date)
		}
	}
	for _, format := range record.formats {
		match := catalogPagePattern.FindStringSubmatch(format)
		if len(match) == 2 {
			parts = append(parts, match[1]+" pages")
			break
		}
	}

	var p repo.BarcodeProduct
	p.Barcode = isbn13
	p.SearchEngineName = dnbSearchEngineName
	p.Item.Name = title
	p.Manufacturer = strings.Join(authors, ", ")
	p.Item.Description = withISBN(strings.Join(parts, " | "), isbn13)
	return p, true
}

func lookupDNB(isbn13 string) ([]repo.BarcodeProduct, error) {
	query := url.Values{}
	query.Set("version", "1.1")
	query.Set("operation", "searchRetrieve")
	query.Set("query", "isbn="+isbn13)
	query.Set("maximumRecords", "1")
	query.Set("recordSchema", "oai_dc")

	req, err := http.NewRequest(http.MethodGet, "https://services.dnb.de/sru/dnb?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Homebox/1.0 (https://github.com/sysadminsmedia/homebox)")

	resp, err := barcodeHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("dnb.de API returned status code: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, barcodeMaxResponseBytes))
	if err != nil {
		return nil, err
	}
	record, records, err := parseDNBSRU(body)
	if err != nil {
		log.Error().Msg("Can not read dnb.de response")
		return nil, err
	}
	if records == 0 {
		return nil, nil
	}
	product, ok := buildDNBBarcodeProduct(isbn13, record)
	if !ok {
		return nil, nil
	}
	return []repo.BarcodeProduct{product}, nil
}

// fetchImageBase64 fetches an image from the given HTTPS URL and returns it as a base64-encoded data URI.
func fetchImageBase64(imageURL string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, imageURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Homebox/1.0 (https://github.com/sysadminsmedia/homebox)")
	if parsed, err := url.Parse(imageURL); err == nil && isAllowedGoogleBooksImageHost(parsed.Hostname()) {
		// Google serves these cover URLs only when the request comes from Books.
		req.Header.Set("Referer", "https://books.google.com/")
	}

	res, err := barcodeHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = res.Body.Close()
	}()

	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("image fetch returned status %d", res.StatusCode)
	}

	contentType := res.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, "image/") {
		return "", fmt.Errorf("non-image content type: %s", contentType)
	}

	limitedReader := io.LimitReader(res.Body, 8*1024*1024)
	raw, err := io.ReadAll(limitedReader)
	if err != nil {
		return "", err
	}

	mimeType := http.DetectContentType(raw)
	var prefix string
	switch mimeType {
	case "image/jpeg":
		prefix = "data:image/jpeg;base64,"
	case "image/png":
		prefix = "data:image/png;base64,"
	default:
		return "", fmt.Errorf("unsupported image type: %s", mimeType)
	}

	// Encode straight into a single buffer sized for prefix+payload so the data URI
	// is built without an intermediate base64 string and a second concat.
	out := make([]byte, len(prefix)+base64.StdEncoding.EncodedLen(len(raw)))
	copy(out, prefix)
	base64.StdEncoding.Encode(out[len(prefix):], raw)

	return string(out), nil
}

// HandleProductSearchFromBarcode godoc
//
//	@Summary	Search EAN from Barcode
//	@Tags		Items
//	@Produce	json
//	@Param		data	query		string	false	"barcode to be searched"
//	@Success	200		{object}	[]repo.BarcodeProduct
//	@Router		/v1/products/search-from-barcode [GET]
//	@Security	Bearer
func (ctrl *V1Controller) HandleProductSearchFromBarcode(conf config.BarcodeAPIConf) errchain.HandlerFunc {
	type query struct {
		// 80 characters is the longest non-2D barcode length (GS1-128)
		EAN string `schema:"productEAN" validate:"required,max=80"`
	}

	return func(w http.ResponseWriter, r *http.Request) error {
		q, err := adapters.DecodeQuery[query](r)
		if err != nil {
			return err
		}

		log.Info().Msg("Processing barcode lookup request on: " + q.EAN)
		// A provider timeout must not consume the server's write deadline and
		// prevent already available results from reaching the client.
		allowSlowResponse(w, r)

		// Spaces and dashes are ignored. An ISBN-10 is searched as its ISBN-13
		// so every provider sees one code. Book databases are only asked for
		// real ISBNs.
		searchCode := compactBarcode(q.EAN)
		isbn13, isISBN := canonicalISBN13(searchCode)
		if isISBN {
			searchCode = isbn13
		}
		if searchCode == "" {
			return server.JSON(w, http.StatusNoContent, nil)
		}

		// www.ean-search.org/: not free

		// Example code: dewalt 5035048748428
		type lookup struct {
			name string
			run  func() ([]repo.BarcodeProduct, error)
		}
		lookups := []lookup{{
			name: "upcitemdb.com",
			run:  func() ([]repo.BarcodeProduct, error) { return lookupUPCItemDB(searchCode) },
		}}

		if conf.TokenBarcodespider != "" {
			lookups = append(lookups, lookup{
				name: "barcodespider.com",
				run: func() ([]repo.BarcodeProduct, error) {
					return lookupBarcodespider(conf.TokenBarcodespider, searchCode)
				},
			})
		}

		for _, source := range openFactsSources {
			lookups = append(lookups, lookup{
				name: source.Name,
				run: func() ([]repo.BarcodeProduct, error) {
					return lookupOpenFacts(conf.OpenFoodFactsContact, source, searchCode)
				},
			})
		}

		if isISBN {
			lookups = append(lookups,
				lookup{
					name: "openlibrary.org",
					run: func() ([]repo.BarcodeProduct, error) {
						return lookupOpenLibrary(conf.OpenFoodFactsContact, isbn13)
					},
				},
				lookup{
					name: googleBooksImageHost,
					run: func() ([]repo.BarcodeProduct, error) {
						return lookupGoogleBooks(conf.TokenGoogleBooks, isbn13)
					},
				},
				lookup{
					name: dnbSearchEngineName,
					run: func() ([]repo.BarcodeProduct, error) {
						return lookupDNB(isbn13)
					},
				},
			)
		}

		results := make([][]repo.BarcodeProduct, len(lookups))
		var wg sync.WaitGroup
		for i, lookup := range lookups {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ps, err := lookup.run()
				if err != nil {
					log.Error().Msg("Can not retrieve product from " + lookup.name + ": " + err.Error())
				}
				results[i] = ps
			}()
		}
		wg.Wait()

		products := make([]repo.BarcodeProduct, 0, len(results))
		for _, result := range results {
			products = append(products, result...)
		}
		for i := range products {
			if products[i].Barcode != "" {
				if strings.TrimSpace(products[i].ModelNumber) == "" {
					products[i].ModelNumber = products[i].Barcode
				}
				if strings.TrimSpace(products[i].Item.ModelNumber) == "" {
					products[i].Item.ModelNumber = products[i].Barcode
				}
			}
			if strings.TrimSpace(products[i].Item.Notes) == "" {
				if label := sourceLabel(products[i].SearchEngineName); label != "" {
					products[i].Item.Notes = "Source: " + label
				}
			}
		}

		// Retrieve images if possible
		for i := range products {
			p := &products[i]

			if len(p.ImageURL) == 0 {
				continue
			}

			// Validate URL is HTTPS. A failed fetch clears the URL so the create
			// form does not attach an empty photo.
			u, err := url.Parse(p.ImageURL)
			if err != nil || u.Scheme != schemeHTTPS {
				log.Warn().Msg("Skipping non-HTTPS image URL: " + p.ImageURL)
				p.ImageURL = ""
				continue
			}

			base64Img, err := fetchImageBase64(p.ImageURL)
			if err != nil {
				log.Warn().Msg("Cannot fetch image for URL: " + p.ImageURL + ": " + err.Error())
				p.ImageURL = ""
				continue
			}
			p.ImageBase64 = base64Img
		}

		// One source often has the edition and another has the cover. Share a
		// fetched cover with the other rows for the same ISBN.
		coverByBarcode := make(map[string]int, len(products))
		for i := range products {
			if products[i].Barcode != "" && products[i].ImageBase64 != "" {
				coverByBarcode[products[i].Barcode] = i
			}
		}
		for i := range products {
			if products[i].ImageBase64 != "" || products[i].Barcode == "" {
				continue
			}
			src, ok := coverByBarcode[products[i].Barcode]
			if !ok {
				continue
			}
			products[i].ImageURL = products[src].ImageURL
			products[i].ImageBase64 = products[src].ImageBase64
		}

		if len(products) != 0 {
			return server.JSON(w, http.StatusOK, products)
		}

		return server.JSON(w, http.StatusNoContent, nil)
	}
}
