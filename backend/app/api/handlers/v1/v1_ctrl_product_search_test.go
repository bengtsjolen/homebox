package v1

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sysadminsmedia/homebox/backend/internal/data/repo"
	"github.com/sysadminsmedia/homebox/backend/internal/sys/config"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestBarcodeSearchReturnsAvailableProviderResults(t *testing.T) {
	originalClient := barcodeHTTPClient
	started := make(chan string, 1+len(openFactsSources))
	release := make(chan struct{})
	barcodeHTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		started <- r.URL.Host
		<-release

		status := http.StatusNotFound
		body := ""
		if r.URL.Host == "api.upcitemdb.com" {
			status = http.StatusOK
			body = `{"code":"OK","items":[{"ean":"1234567890123","title":"Available product"}]}`
		}
		return &http.Response{
			StatusCode: status,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    r,
		}, nil
	})}
	t.Cleanup(func() { barcodeHTTPClient = originalClient })

	handler := (&V1Controller{}).HandleProductSearchFromBarcode(config.BarcodeAPIConf{})
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := handler(w, r); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}))
	server.Config.WriteTimeout = 50 * time.Millisecond
	server.Start()
	t.Cleanup(server.Close)

	type response struct {
		status int
		body   []byte
		err    error
	}
	responseCh := make(chan response, 1)
	go func() {
		resp, err := server.Client().Get(server.URL + "/?productEAN=1234567890123")
		if err != nil {
			responseCh <- response{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		responseCh <- response{status: resp.StatusCode, body: body, err: err}
	}()

	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for count := 0; count < cap(started); count++ {
		select {
		case <-started:
		case <-timer.C:
			close(release)
			<-responseCh
			t.Fatalf("only %d of %d barcode providers started before the first response", count, cap(started))
		}
	}

	time.Sleep(75 * time.Millisecond)
	close(release)
	result := <-responseCh
	if result.err != nil {
		t.Fatalf("barcode search failed after a provider delay: %v", result.err)
	}
	if result.status != http.StatusOK {
		t.Fatalf("expected status 200, got %d", result.status)
	}

	var products []repo.BarcodeProduct
	if err := json.Unmarshal(result.body, &products); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(products) != 1 || products[0].Item.Name != "Available product" {
		t.Fatalf("unexpected products: %+v", products)
	}
}

func TestUPCITEMDBResponseUnmarshalNumericListPrice(t *testing.T) {
	body := []byte(`{
		"code": "OK",
		"total": 1,
		"offset": 0,
		"items": [{
			"title": "Example",
			"offers": [{
				"merchant": "ExampleStore",
				"list_price": 19.99,
				"price": 14.5,
				"shipping": 4.25
			}]
		}]
	}`)

	var result UPCITEMDBResponse
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("unexpected unmarshal error: %v", err)
	}

	if len(result.Items) != 1 || len(result.Items[0].Offers) != 1 {
		t.Fatalf("expected one item with one offer, got items=%d", len(result.Items))
	}

	offer := result.Items[0].Offers[0]
	if offer.ListPrice != "19.99" {
		t.Fatalf("expected list_price %q, got %q", "19.99", offer.ListPrice)
	}
	if offer.Shipping != "4.25" {
		t.Fatalf("expected shipping %q, got %q", "4.25", offer.Shipping)
	}
}

func TestUPCITEMDBResponseUnmarshalStringListPrice(t *testing.T) {
	body := []byte(`{
		"items": [{
			"offers": [{
				"list_price": "19.99",
				"shipping": "Free"
			}]
		}]
	}`)

	var result UPCITEMDBResponse
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("unexpected unmarshal error: %v", err)
	}

	if len(result.Items) == 0 || len(result.Items[0].Offers) == 0 {
		t.Fatalf("expected at least one item with one offer, got items=%d", len(result.Items))
	}

	offer := result.Items[0].Offers[0]
	if offer.ListPrice != "19.99" {
		t.Fatalf("expected list_price %q, got %q", "19.99", offer.ListPrice)
	}
	if offer.Shipping != "Free" {
		t.Fatalf("expected shipping %q, got %q", "Free", offer.Shipping)
	}
}

func TestUPCITEMDBResponseUnmarshalNullListPrice(t *testing.T) {
	body := []byte(`{
		"items": [{
			"offers": [{
				"list_price": null,
				"shipping": null
			}]
		}]
	}`)

	var result UPCITEMDBResponse
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("unexpected unmarshal error: %v", err)
	}

	if len(result.Items) == 0 || len(result.Items[0].Offers) == 0 {
		t.Fatalf("expected at least one item with one offer, got items=%d", len(result.Items))
	}

	offer := result.Items[0].Offers[0]
	if offer.ListPrice != "" {
		t.Fatalf("expected empty list_price, got %q", offer.ListPrice)
	}
	if offer.Shipping != "" {
		t.Fatalf("expected empty shipping, got %q", offer.Shipping)
	}
}

func TestFlexibleStringRejectsCompositeTypes(t *testing.T) {
	cases := map[string]string{
		"object": `{"foo":"bar"}`,
		"array":  `[1,2,3]`,
		"bool":   `true`,
	}

	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			var f flexibleString
			if err := f.UnmarshalJSON([]byte(payload)); err == nil {
				t.Fatalf("expected error for %s payload, got nil (value=%q)", name, f)
			}
		})
	}
}

func TestFlexibleStringHandlesLeadingWhitespace(t *testing.T) {
	var f flexibleString
	if err := f.UnmarshalJSON([]byte("   12.50")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f != "12.50" {
		t.Fatalf("expected %q, got %q", "12.50", f)
	}
}

func TestBuildOpenFactsBarcodeProduct(t *testing.T) {
	product, ok := buildOpenFactsBarcodeProduct("openbeautyfacts.org", "3600522058124", openFactsProduct{
		ProductName:   "Shower Gel",
		Brands:        "Example Brand",
		GenericName:   "Body wash",
		Categories:    "Hygiene, Shower",
		Quantity:      "250 ml",
		ImageFrontURL: "http://images.openbeautyfacts.org/images/products/360/052/205/8124/front_en.1.400.jpg",
	})

	if !ok {
		t.Fatal("expected product to be built")
	}
	if product.Barcode != "3600522058124" {
		t.Fatalf("unexpected barcode: %s", product.Barcode)
	}
	if product.SearchEngineName != "openbeautyfacts.org" {
		t.Fatalf("unexpected source name: %s", product.SearchEngineName)
	}
	if product.Item.Name != "Shower Gel" {
		t.Fatalf("unexpected product name: %s", product.Item.Name)
	}
	if product.Manufacturer != "Example Brand" {
		t.Fatalf("unexpected manufacturer: %s", product.Manufacturer)
	}
	if product.Item.Description != "Body wash | Hygiene, Shower | 250 ml" {
		t.Fatalf("unexpected description: %s", product.Item.Description)
	}
	if product.ImageURL != "https://images.openbeautyfacts.org/images/products/360/052/205/8124/front_en.1.400.jpg" {
		t.Fatalf("unexpected image URL: %s", product.ImageURL)
	}
}

func TestBuildOpenFactsBarcodeProductFallsBackToGenericName(t *testing.T) {
	product, ok := buildOpenFactsBarcodeProduct("openproductsfacts.org", "1234567890123", openFactsProduct{
		GenericName: "Replacement filter",
		Categories:  "Appliance parts",
	})

	if !ok {
		t.Fatal("expected product to be built")
	}
	if product.Item.Name != "Replacement filter" {
		t.Fatalf("unexpected product name: %s", product.Item.Name)
	}
	if product.Item.Description != "Appliance parts" {
		t.Fatalf("unexpected description: %s", product.Item.Description)
	}
}

func TestBuildOpenFactsBarcodeProductRequiresName(t *testing.T) {
	_, ok := buildOpenFactsBarcodeProduct("openfoodfacts.org", "1234567890123", openFactsProduct{})
	if ok {
		t.Fatal("expected empty product to be ignored")
	}
}

func TestBuildOpenFactsBarcodeProductRejectsUntrustedImageHost(t *testing.T) {
	product, ok := buildOpenFactsBarcodeProduct("openfoodfacts.org", "1234567890123", openFactsProduct{
		ProductName: "Example Product",
		ImageURL:    "https://example.com/image.jpg",
	})

	if !ok {
		t.Fatal("expected product to be built")
	}
	if product.ImageURL != "" {
		t.Fatalf("expected untrusted image URL to be cleared, got %q", product.ImageURL)
	}
}

func TestBuildOpenFactsBarcodeProductRejectsUnsupportedImageScheme(t *testing.T) {
	product, ok := buildOpenFactsBarcodeProduct("openfoodfacts.org", "1234567890123", openFactsProduct{
		ProductName: "Example Product",
		ImageURL:    "ftp://images.openfoodfacts.org/image.jpg",
	})

	if !ok {
		t.Fatal("expected product to be built")
	}
	if product.ImageURL != "" {
		t.Fatalf("expected unsupported image URL to be cleared, got %q", product.ImageURL)
	}
}

func TestSanitizeHeaderRemovesControlCharacters(t *testing.T) {
	got := sanitizeHeader("owner@example.com\r\nInjected: value\t")
	if got != "owner@example.comInjected: value" {
		t.Fatalf("unexpected sanitized header: %q", got)
	}
}

func TestCanonicalISBN13(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{in: "0-306-40615-2", want: "9780306406157", ok: true},
		{in: "978-0-306-40615-7", want: "9780306406157", ok: true},
		{in: "080442957X", want: "9780804429573", ok: true},
		{in: "080442957x", want: "9780804429573", ok: true},
		{in: "9780306406158", want: "", ok: false},
		{in: "1234567890123", want: "", ok: false},
		{in: "5035048748428", want: "", ok: false},
	}

	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := canonicalISBN13(compactBarcode(tc.in))
			if ok != tc.ok || got != tc.want {
				t.Fatalf("got %q ok=%v, want %q ok=%v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestBuildOpenLibraryBarcodeProduct(t *testing.T) {
	var book openLibraryBook
	if err := json.Unmarshal([]byte(`{
		"title": "Concrete Mathematics",
		"authors": [{"name": "Ronald L. Graham"}, {"name": " Donald E. Knuth "}],
		"publishers": [{"name": "Addison-Wesley"}],
		"publish_date": "1994",
		"number_of_pages": 657,
		"cover": {"large": "http://covers.openlibrary.org/b/id/135182-L.jpg"}
	}`), &book); err != nil {
		t.Fatalf("unmarshal book: %v", err)
	}

	product, ok := buildOpenLibraryBarcodeProduct("9780306406157", book)
	if !ok {
		t.Fatal("expected product to be built")
	}
	if product.Item.Name != "Concrete Mathematics" {
		t.Fatalf("unexpected title: %s", product.Item.Name)
	}
	if product.Manufacturer != "Ronald L. Graham, Donald E. Knuth" {
		t.Fatalf("unexpected authors: %s", product.Manufacturer)
	}
	if product.Item.Description != "Addison-Wesley | 1994 | 657 pages | ISBN 9780306406157" {
		t.Fatalf("unexpected description: %s", product.Item.Description)
	}
	if product.SearchEngineName != "openlibrary.org" {
		t.Fatalf("unexpected source: %s", product.SearchEngineName)
	}
	if product.ImageURL != "https://covers.openlibrary.org/b/id/135182-L.jpg" {
		t.Fatalf("unexpected cover: %s", product.ImageURL)
	}
}

func TestBuildOpenLibraryBarcodeProductFallsBackToISBNCover(t *testing.T) {
	product, ok := buildOpenLibraryBarcodeProduct("9780306406157", openLibraryBook{
		Title: "Concrete Mathematics",
		Cover: struct {
			Small  string `json:"small"`
			Medium string `json:"medium"`
			Large  string `json:"large"`
		}{Large: "https://example.com/cover.jpg"},
	})
	if !ok {
		t.Fatal("expected product to be built")
	}
	if product.ImageURL != "https://covers.openlibrary.org/b/isbn/9780306406157-L.jpg?default=false" {
		t.Fatalf("unexpected cover: %s", product.ImageURL)
	}
}

func TestBuildOpenLibraryBarcodeProductRequiresTitle(t *testing.T) {
	_, ok := buildOpenLibraryBarcodeProduct("9780306406157", openLibraryBook{})
	if ok {
		t.Fatal("expected a book without a title to be ignored")
	}
}

func TestBarcodeSearchIncludesOpenLibraryForISBN(t *testing.T) {
	originalClient := barcodeHTTPClient
	barcodeHTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		status := http.StatusNotFound
		body := ""
		if r.URL.Host == "openlibrary.org" {
			if !strings.Contains(r.Header.Get("User-Agent"), "Homebox/1.0 (contact: owner@example.com)") {
				t.Errorf("unexpected user agent %q", r.Header.Get("User-Agent"))
			}
			if r.URL.Query().Get("bibkeys") != "ISBN:9780306406157" {
				t.Errorf("unexpected bibkeys %q", r.URL.Query().Get("bibkeys"))
			}
			status = http.StatusOK
			body = `{
				"ISBN:9780306406157": {
					"title": "Concrete Mathematics",
					"authors": [{"name": "Ronald L. Graham"}],
					"publishers": [{"name": "Addison-Wesley"}],
					"publish_date": "1994",
					"number_of_pages": 657,
					"cover": {"large": "https://covers.openlibrary.org/b/id/135182-L.jpg"}
				}
			}`
		}

		return &http.Response{
			StatusCode: status,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    r,
		}, nil
	})}
	t.Cleanup(func() { barcodeHTTPClient = originalClient })

	handler := (&V1Controller{}).HandleProductSearchFromBarcode(config.BarcodeAPIConf{
		OpenFoodFactsContact: "owner@example.com",
	})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/?productEAN=0-306-40615-2", nil)
	if err := handler(recorder, request); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d (%s)", recorder.Code, recorder.Body.String())
	}

	var products []repo.BarcodeProduct
	if err := json.Unmarshal(recorder.Body.Bytes(), &products); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(products) != 1 {
		t.Fatalf("expected one product, got %+v", products)
	}
	if products[0].SearchEngineName != "openlibrary.org" || products[0].Item.Name != "Concrete Mathematics" {
		t.Fatalf("unexpected product: %+v", products[0])
	}
	if products[0].Barcode != "9780306406157" {
		t.Fatalf("expected canonical ISBN, got %s", products[0].Barcode)
	}
}

func TestBuildGoogleBooksBarcodeProduct(t *testing.T) {
	var volume googleBooksVolume
	if err := json.Unmarshal([]byte(`{
		"volumeInfo": {
			"title": "Kraftwerk",
			"subtitle": "Future Music from Germany",
			"authors": ["Uwe Schütte"],
			"publisher": "Penguin Books",
			"publishedDate": "2020",
			"description": "A <b>history</b> of Kraftwerk.",
			"pageCount": 316,
			"imageLinks": {
				"thumbnail": "http://books.google.com/books/content?id=abc&printsec=frontcover&img=1&zoom=1"
			},
			"industryIdentifiers": [{"type": "ISBN_13", "identifier": "9781802060980"}]
		}
	}`), &volume); err != nil {
		t.Fatalf("unmarshal volume: %v", err)
	}

	product, ok := buildGoogleBooksBarcodeProduct("9781802060980", volume)
	if !ok {
		t.Fatal("expected product to be built")
	}
	if product.Item.Name != "Kraftwerk: Future Music from Germany" {
		t.Fatalf("unexpected title: %s", product.Item.Name)
	}
	if product.Manufacturer != "Uwe Schütte" {
		t.Fatalf("unexpected author: %s", product.Manufacturer)
	}
	if product.Item.Description != "A history of Kraftwerk. | ISBN 9781802060980" {
		t.Fatalf("unexpected description: %s", product.Item.Description)
	}
	if product.SearchEngineName != "books.google.com" {
		t.Fatalf("unexpected source: %s", product.SearchEngineName)
	}

	imageURL, err := url.Parse(product.ImageURL)
	if err != nil {
		t.Fatalf("parse cover: %v", err)
	}
	if imageURL.Scheme != "https" || imageURL.Host != "books.google.com" || imageURL.Query().Get("zoom") != "3" {
		t.Fatalf("unexpected cover: %s", product.ImageURL)
	}
}

func TestBuildGoogleBooksBarcodeProductRejectsUntrustedImage(t *testing.T) {
	volume := googleBooksVolume{}
	volume.VolumeInfo.Title = "Kraftwerk"
	volume.VolumeInfo.ImageLinks.Thumbnail = "https://example.com/cover.jpg"
	product, ok := buildGoogleBooksBarcodeProduct("9781802060980", volume)
	if !ok {
		t.Fatal("expected product to be built")
	}
	if product.ImageURL != "" {
		t.Fatalf("expected untrusted cover to be cleared, got %s", product.ImageURL)
	}
}

func TestGoogleBooksSiblingCoverUsesSameYearEdition(t *testing.T) {
	source := googleBooksVolume{}
	source.VolumeInfo.Title = "Kraftwerk"
	source.VolumeInfo.Subtitle = "Future Music from Germany"
	source.VolumeInfo.Authors = []string{"Uwe Schütte"}
	source.VolumeInfo.PublishedDate = "2020"
	source.VolumeInfo.IndustryIdentifiers = []struct {
		Type       string `json:"type"`
		Identifier string `json:"identifier"`
	}{{Type: "ISBN_13", Identifier: "9781802060980"}}

	otherYear := googleBooksVolume{}
	otherYear.VolumeInfo.Title = "Kraftwerk: Future Music from Germany"
	otherYear.VolumeInfo.Authors = []string{"Schütte, Uwe"}
	otherYear.VolumeInfo.PublishedDate = "2017"
	otherYear.VolumeInfo.ImageLinks.Thumbnail = "https://books.google.com/books/content?id=old&zoom=1"

	sameYear := googleBooksVolume{}
	sameYear.VolumeInfo.Title = "Kraftwerk: Future Music from Germany"
	sameYear.VolumeInfo.Authors = []string{"Uwe Schütte"}
	sameYear.VolumeInfo.PublishedDate = "2020-03"
	sameYear.VolumeInfo.IndustryIdentifiers = []struct {
		Type       string `json:"type"`
		Identifier string `json:"identifier"`
	}{{Type: "ISBN_13", Identifier: "9780141986753"}}
	sameYear.VolumeInfo.ImageLinks.Thumbnail = "https://books.google.com/books/content?id=same&zoom=1"

	cover := googleBooksSiblingCover("9781802060980", source, []googleBooksVolume{source, otherYear, sameYear})
	if !strings.Contains(cover, "id=same") {
		t.Fatalf("expected the 2020 edition cover, got %s", cover)
	}
}

func TestBarcodeSearchUsesGoogleBooksKeyAndSiblingCover(t *testing.T) {
	originalClient := barcodeHTTPClient
	jpeg := []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00\x01\x01\x00\x00\x01\x00\x01\x00\x00\xff\xd9")
	barcodeHTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "www.googleapis.com" && r.URL.Host != "books.google.com" {
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    r,
			}, nil
		}
		if r.URL.Host == "books.google.com" {
			header := make(http.Header)
			header.Set("Content-Type", "image/jpeg")
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     header,
				Body:       io.NopCloser(strings.NewReader(string(jpeg))),
				Request:    r,
			}, nil
		}
		if r.URL.Query().Get("key") != "test-key" {
			t.Errorf("expected API key, got %q", r.URL.Query().Get("key"))
		}

		body := ""
		switch r.URL.Query().Get("q") {
		case "isbn:9781802060980":
			body = `{
				"items": [{
					"volumeInfo": {
						"title": "Kraftwerk",
						"subtitle": "Future Music from Germany",
						"authors": ["Uwe Schütte"],
						"publishedDate": "2020",
						"industryIdentifiers": [{"type": "ISBN_13", "identifier": "9781802060980"}]
					}
				}]
			}`
		default:
			if !strings.Contains(r.URL.Query().Get("q"), "intitle:Kraftwerk") {
				t.Errorf("unexpected related query %q", r.URL.Query().Get("q"))
			}
			body = `{
				"items": [{
					"volumeInfo": {
						"title": "Kraftwerk: Future Music from Germany",
						"authors": ["Uwe Schütte"],
						"publishedDate": "2020",
						"imageLinks": {"thumbnail": "https://books.google.com/books/content?id=same&zoom=1"},
						"industryIdentifiers": [{"type": "ISBN_13", "identifier": "9780141986753"}]
					}
				}]
			}`
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    r,
		}, nil
	})}
	t.Cleanup(func() { barcodeHTTPClient = originalClient })

	handler := (&V1Controller{}).HandleProductSearchFromBarcode(config.BarcodeAPIConf{
		TokenGoogleBooks: "test-key",
	})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/?productEAN=9781802060980", nil)
	if err := handler(recorder, request); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d (%s)", recorder.Code, recorder.Body.String())
	}

	var products []repo.BarcodeProduct
	if err := json.Unmarshal(recorder.Body.Bytes(), &products); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	var google *repo.BarcodeProduct
	for i := range products {
		if products[i].SearchEngineName == "books.google.com" {
			google = &products[i]
		}
	}
	if google == nil {
		t.Fatalf("google books result missing: %+v", products)
	}
	if google.ImageBase64 == "" {
		t.Fatal("expected the sibling edition cover")
	}
	if google.Item.Name != "Kraftwerk: Future Music from Germany" {
		t.Fatalf("unexpected title: %s", google.Item.Name)
	}
}

func TestWithISBNKeepsTheNumberInsideTheDescriptionLimit(t *testing.T) {
	body := strings.Repeat("word ", 400)
	got := withISBN(body, "9781802060980")
	if len([]rune(got)) > barcodeDescriptionLimit {
		t.Fatalf("description length %d exceeds %d", len([]rune(got)), barcodeDescriptionLimit)
	}
	if !strings.HasSuffix(got, "ISBN 9781802060980") {
		t.Fatalf("description lost the ISBN: %s", got[len(got)-40:])
	}
}

func TestBarcodeSearchUsesGoogleBooksWhenOpenLibraryMisses(t *testing.T) {
	originalClient := barcodeHTTPClient
	jpeg := []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00\x01\x01\x00\x00\x01\x00\x01\x00\x00\xff\xd9")
	barcodeHTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		status := http.StatusNotFound
		body := ""
		header := make(http.Header)

		switch r.URL.Host {
		case "openlibrary.org":
			status = http.StatusOK
			body = `{}`
		case "www.googleapis.com":
			if r.URL.Query().Get("q") != "isbn:9781802060980" {
				t.Errorf("unexpected google query %q", r.URL.Query().Get("q"))
			}
			status = http.StatusOK
			body = `{
				"items": [{
					"volumeInfo": {
						"title": "Kraftwerk",
						"subtitle": "Future Music from Germany",
						"authors": ["Uwe Schütte"],
						"publisher": "Penguin Books",
						"publishedDate": "2020",
						"pageCount": 316,
						"imageLinks": {
							"thumbnail": "https://books.google.com/books/content?id=abc&zoom=1"
						},
						"industryIdentifiers": [{"type": "ISBN_13", "identifier": "978-1-80206-098-0"}]
					}
				}]
			}`
		case "books.google.com":
			status = http.StatusOK
			header.Set("Content-Type", "image/jpeg")
			body = string(jpeg)
		}

		return &http.Response{
			StatusCode: status,
			Header:     header,
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    r,
		}, nil
	})}
	t.Cleanup(func() { barcodeHTTPClient = originalClient })

	handler := (&V1Controller{}).HandleProductSearchFromBarcode(config.BarcodeAPIConf{})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/?productEAN=9781802060980", nil)
	if err := handler(recorder, request); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d (%s)", recorder.Code, recorder.Body.String())
	}

	var products []repo.BarcodeProduct
	if err := json.Unmarshal(recorder.Body.Bytes(), &products); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(products) != 1 || products[0].SearchEngineName != "books.google.com" {
		t.Fatalf("unexpected products: %+v", products)
	}
	if products[0].Item.Name != "Kraftwerk: Future Music from Germany" || products[0].Manufacturer != "Uwe Schütte" {
		t.Fatalf("unexpected book: %+v", products[0])
	}
	if products[0].ImageBase64 == "" {
		t.Fatal("expected the Google Books cover to be attached")
	}
}

func TestBuildDNBBarcodeProductFromSRU(t *testing.T) {
	record, records, err := parseDNBSRU([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<searchRetrieveResponse>
  <numberOfRecords>1</numberOfRecords>
  <records><record><recordData><dc>
    <title>Kraftwerk : future music from Germany / Uwe Schütte</title>
    <creator>Schütte, Uwe [Verfasser]</creator>
    <publisher>London : Penguin Books</publisher>
    <date>2020</date>
    <identifier>978-1-80206-098-0</identifier>
    <format>xv, 316 pages :</format>
  </dc></recordData></record></records>
</searchRetrieveResponse>`))
	if err != nil {
		t.Fatalf("parse SRU: %v", err)
	}
	if records != 1 {
		t.Fatalf("expected one record, got %d", records)
	}

	product, ok := buildDNBBarcodeProduct("9781802060980", record)
	if !ok {
		t.Fatal("expected product to be built")
	}
	if product.Item.Name != "Kraftwerk: future music from Germany" {
		t.Fatalf("unexpected title: %s", product.Item.Name)
	}
	if product.Manufacturer != "Uwe Schütte" {
		t.Fatalf("unexpected author: %s", product.Manufacturer)
	}
	if product.SearchEngineName != "www.dnb.de" {
		t.Fatalf("unexpected source: %s", product.SearchEngineName)
	}
	if product.Item.Description != "London : Penguin Books | 2020 | 316 pages | ISBN 9781802060980" {
		t.Fatalf("unexpected description: %s", product.Item.Description)
	}
}

func TestBarcodeSearchUsesDNBWhenOtherBookSourcesMiss(t *testing.T) {
	originalClient := barcodeHTTPClient
	barcodeHTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		status := http.StatusNotFound
		body := ""
		if r.URL.Host == "services.dnb.de" {
			if !strings.Contains(r.URL.Query().Get("query"), "isbn=9781802060980") {
				t.Errorf("unexpected dnb query %q", r.URL.Query().Get("query"))
			}
			status = http.StatusOK
			body = `<searchRetrieveResponse><numberOfRecords>1</numberOfRecords><records><record><recordData><dc>
				<title>Kraftwerk : future music from Germany / Uwe Schütte</title>
				<creator>Schütte, Uwe [Verfasser]</creator>
				<publisher>London : Penguin Books</publisher>
				<date>2020</date>
			</dc></recordData></record></records></searchRetrieveResponse>`
		}
		return &http.Response{
			StatusCode: status,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    r,
		}, nil
	})}
	t.Cleanup(func() { barcodeHTTPClient = originalClient })

	handler := (&V1Controller{}).HandleProductSearchFromBarcode(config.BarcodeAPIConf{})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/?productEAN=9781802060980", nil)
	if err := handler(recorder, request); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d (%s)", recorder.Code, recorder.Body.String())
	}

	var products []repo.BarcodeProduct
	if err := json.Unmarshal(recorder.Body.Bytes(), &products); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(products) != 1 || products[0].SearchEngineName != "www.dnb.de" {
		t.Fatalf("unexpected products: %+v", products)
	}
	if products[0].Item.Name != "Kraftwerk: future music from Germany" || products[0].Manufacturer != "Uwe Schütte" {
		t.Fatalf("unexpected book: %+v", products[0])
	}
}
