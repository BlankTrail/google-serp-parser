//go:build ignore

// SPDX-License-Identifier: MIT

// Command generate writes the synthetic result pages the parser tests run
// against.
//
// They are written, not captured. Every structure here was measured on live
// sessions first, and each page models exactly one arrangement the parser has
// to survive — the encrypted link form, the card layout with no cite, the
// three ad placements, and the JavaScript shell that arrives with HTTP 200 and
// no results in it.
//
// The attribute names are the real ones because they are what the parser
// anchors on: data-ved marks a telemetry-bearing link, data-text-ad marks a
// text ad, data-pcu carries an advertiser's address in plain text. Their
// values are placeholders — the real ones are session tokens and have no
// business in a public repository.
//
//	go run google/testdata/generate.go
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// result is one organic entry to render.
type result struct {
	title   string
	host    string
	path    string
	snippet string
}

// page describes one fixture.
type page struct {
	file    string
	comment string
	query   string
	results []result
	encoded bool // links go through /goto rather than straight to the host
	cards   bool // titles in [role=heading]; no cite anywhere, as measured
	topAds  []advert
	botAds  []advert
	prodAds []string
	shell   bool
}

// advert is one text ad.
type advert struct {
	title string
	host  string
	body  string
}

func main() {
	dir := filepath.Dir(os.Args[0])
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	for _, p := range pages() {
		out := filepath.Join(dir, p.file)
		if err := os.WriteFile(out, []byte(render(p)), 0o644); err != nil {
			panic(err)
		}
		fmt.Printf("%s (%d bytes)\n", out, len(render(p)))
	}
}

func pages() []page {
	ru := []result{
		{"Как тестировать методы REST API", "habr.com", "ru › articles", "Разбор подходов к тестированию REST API и типичных ошибок."},
		{"Один запрос, шестнадцать проверок", "software-testing.ru", "testing › for-beginners", "Как из одного запроса вытащить максимум проверок."},
		{"Работа с GET", "testgrow.ru", "lectures › lecture59", "Лекция о параметрах GET-запроса и их проверке."},
		{"Тестирование запроса", "www.politerm.com", "zuludoc › zb_query_test", "Документация по проверке запросов в ZuluGIS."},
		{"Как тестировать API в Postman", "gb.ru", "blog › postman", "Пошаговое руководство для начинающих."},
		{"Имеет ли смысл проверять всё", "qna.habr.com", "q › 1234567", "Обсуждение границ разумного покрытия."},
		{"Инструменты тестирования API", "testengineer.ru", "tools", "Обзор инструментов и их сильных сторон."},
		{"Проверка ответов сервера", "apitest.dev", "guides › responses", "Что именно стоит утверждать в ответе."},
		// A Google property under the encrypted form: the destination is not in
		// the page, so the cite is the only place a Google host can show up,
		// and it must be filtered the same way classifyLink filters direct and
		// redirect links — measured to differ by link form otherwise.
		{"Google Search Help", "support.google.com", "websearch › answer", "Справка по операторам поиска Google."},
	}
	cards := []result{
		{"ChatGPT начинает блокировать прямые запросы", "", "", "Новость о смене политики доступа."},
		{"Как проводить нагрузочное тестирование", "", "", "Практическое руководство по нагрузке."},
		{"Burp Suite для тестирования безопасности", "", "", "Разбор возможностей инструмента."},
		{"Как пользоваться SYNTX AI", "", "", "Первые шаги в интерфейсе."},
		{"Автотесты без боли", "", "", "О поддерживаемых автотестах."},
		{"Что должен уметь QA-инженер", "", "", "Список навыков и почему они нужны."},
		{"Пирамида тестирования", "", "", "Классическая модель и её критика."},
		{"Контрактное тестирование", "", "", "Зачем нужны контракты между сервисами."},
		{"Мутационное тестирование", "", "", "Как доказать, что тест краснеет."},
		{"Тест-дизайн на практике", "", "", "Техники и когда они уместны."},
	}
	site := make([]result, 0, 10)
	for i, title := range []string{
		"BlankTrail Proxy — Browser Network Profiles",
		"BlankTrail Proxy Changelog — Release History",
		"BlankTrail Proxy Documentation",
		"Contacts - BlankTrail Proxy",
		"Pricing — BlankTrail Proxy",
		"Challenge Breaker — BlankTrail Proxy",
		"Gateways — BlankTrail Proxy",
		"API Reference — BlankTrail Proxy",
		"Downloads — BlankTrail Proxy",
		"FAQ — BlankTrail Proxy",
	} {
		site = append(site, result{title, "blanktrail.com", fmt.Sprintf("page%d", i+1),
			"Страница сайта BlankTrail Proxy."})
	}
	us := []result{
		{"Used iPhones | Shop Certified Refurbished iPhones", "buy.gazelle.com", "iphone", "Certified refurbished iPhones with a warranty."},
		{"Shop the Newest Apple iPhones - AT&T", "www.att.com", "buy › phones", "Deals on the latest Apple iPhone models."},
		{"Shop Apple iPhone: Find Prices, Specs & Deals", "www.boostmobile.com", "phones › apple", "Compare iPhone prices and plans."},
		{"Apple Cell Phones & Smartphones", "www.ebay.com", "b › apple", "Listings for new and used Apple phones."},
		{"iPhone - Apple", "www.apple.com", "iphone", "The official Apple iPhone page."},
		{"iPhone - Wikipedia", "en.wikipedia.org", "wiki › IPhone", "Encyclopaedia article on the iPhone line."},
		{"New Apple iPhones & Accessories", "www.bestbuy.com", "site › apple-iphone", "Retail listings and accessories."},
		{"Apple iPhone: Shop Apple Smartphones", "www.verizon.com", "smartphones › apple", "Carrier offers on Apple smartphones."},
		{"Refurbished iPhone deals", "swappa.com", "iphone", "Marketplace listings for used iPhones."},
		{"Compare iPhone models", "www.gsmarena.com", "apple-phones", "Specification comparison table."},
	}
	direct := []result{
		{"iPhone", "www.apple.com", "iphone", "The official Apple iPhone page."},
		{"iPhone", "en.wikipedia.org", "wiki › IPhone", "Encyclopaedia article on the iPhone line."},
		{"New Apple iPhones & Accessories", "www.bestbuy.com", "site › apple-iphone", "Retail listings and accessories."},
		{"Apple iPhone: Shop Apple Smartphones", "www.verizon.com", "smartphones › apple", "Carrier offers on Apple smartphones."},
		{"Apple iPhone deals", "www.t-mobile.com", "cell-phone › apple", "Carrier promotions on iPhone."},
		{"Buy iPhone unlocked", "www.walmart.com", "browse › iphone", "Retail listings for unlocked phones."},
		{"iPhone accessories", "www.target.com", "c › iphone", "Cases, cables and chargers."},
		{"Trade in your iPhone", "www.gazelle.com", "trade-in", "Trade-in values by model."},
	}

	return []page{
		{
			file:    "serp_goto_ru.html",
			comment: "encrypted /goto links, titles in h3, cite present",
			query:   "тестовый запрос", results: ru, encoded: true,
		},
		{
			file:    "serp_cards_ru.html",
			comment: "card layout: no h3 and no cite anywhere; titles in [role=heading]",
			query:   "тестовый запрос", results: cards, encoded: true, cards: true,
		},
		{
			file:    "serp_site_ru.html",
			comment: "a site: query — the shape the index check reads",
			query:   "site:blanktrail.com", results: site, encoded: true,
		},
		{
			file:    "serp_ads_us.html",
			comment: "all three ad placements at once: top, bottom and product",
			query:   "buy iphone", results: us, encoded: true,
			topAds: []advert{
				{"Cricket Wireless® iPhone promo", "https://www.cricketwireless.com/", "Switch and save on iPhone."},
				{"Certified pre-owned iPhone 14", "https://patriotmobile.com/", "In stock and ready to ship."},
				{"iPhones, iPads, MacBooks", "https://www.adorama.com/", "Shop Apple at Adorama."},
				{"Apple iPhone Models", "https://savings.consumercellular.com/", "Compare models and plans."},
			},
			botAds: []advert{
				{"Refurbished iPhones For Sale", "https://www.plug.tech/", "Unlocked and tested."},
				{"certified pre-owned iphone se 3", "https://patriotmobile.com/", "Devices with warranty."},
				{"Used & Refurbished iPhones", "https://reebelo.com/", "Free delivery, 30-day returns."},
			},
			prodAds: []string{"Apple iPhone 15 — 676,99 $ — Reebelo USA", "Apple iPhone 14 — 218,99 $ — Gen Mobile", "Apple iPhone 13 — 1 099,00 $ — Best Buy"},
		},
		{
			file:    "serp_direct_us.html",
			comment: "plain href links instead of the encrypted redirector",
			query:   "iphone", results: direct,
		},
		{
			file:    "jsshell.html",
			comment: "the JavaScript shell served to a plain client: HTTP 200, no results, ever",
			query:   "iphone", shell: true,
		},
	}
}

func render(p page) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<!doctype html>\n<!-- synthetic fixture: %s -->\n", p.comment)
	fmt.Fprintf(&b, "<html><head><title>%s</title></head><body>\n", esc(p.query))

	if p.shell {
		// The shell is a valid page that tells the visitor to enable
		// JavaScript and carries no result markup whatsoever. That is the
		// whole point: it is HTTP 200 and it is not results.
		b.WriteString(`<div id="main"><div>Enable JavaScript and cookies to continue</div></div>` + "\n")
		b.WriteString("</body></html>\n")
		return b.String()
	}

	b.WriteString(`<div id="search"><div id="rso">` + "\n")
	for i, r := range p.results {
		b.WriteString(renderResult(p, i, r))
	}
	b.WriteString("</div></div>\n")

	if len(p.topAds) > 0 {
		b.WriteString(`<div id="taw"><div id="tvcap"><div id="tads">` + "\n")
		for i, a := range p.topAds {
			b.WriteString(renderAd(i, a))
		}
		b.WriteString("</div></div></div>\n")
	}
	if len(p.botAds) > 0 {
		b.WriteString(`<div id="bottomads">` + "\n")
		for i, a := range p.botAds {
			b.WriteString(renderAd(i, a))
		}
		b.WriteString("</div>\n")
	}
	if len(p.prodAds) > 0 {
		b.WriteString(`<div id="rhs" role="complementary">` + "\n")
		for i, t := range p.prodAds {
			fmt.Fprintf(&b, `<a href="/aclk?ad=%d" data-ved="x"><h3>%s</h3></a>`+"\n", i, esc(t))
		}
		b.WriteString("</div>\n")
	}

	// Related searches are links back into search, which is what identifies
	// them: everything else on the page points outwards.
	b.WriteString(`<div id="botstuff">` + "\n")
	for _, rel := range []struct{ q, text string }{
		{"related one", "related one"},
		{"related two", "related two"},
		// One real chip's anchor text arrives out of visual order — measured
		// on a real page — so the phrase must come from the query parameter,
		// never from the anchor text.
		{"related three", "threerelated"},
	} {
		fmt.Fprintf(&b, `<a href="/search?q=%s" data-ved="x">%s</a>`+"\n", esc(rel.q), esc(rel.text))
	}
	// The pagination bar: real captures show it living in the very same
	// container as the related-search chips, answering to the same href
	// prefix, so it must be excluded by structure (role="navigation" and the
	// start parameter) rather than by counting or position.
	b.WriteString(`<div role="navigation">` + "\n")
	for pn := 2; pn <= 5; pn++ {
		fmt.Fprintf(&b, `<a href="/search?q=%s&amp;start=%d">%d</a>`+"\n", esc(p.query), (pn-1)*10, pn)
	}
	b.WriteString("</div>\n")
	b.WriteString("</div>\n</body></html>\n")
	return b.String()
}

func renderResult(p page, i int, r result) string {
	href := fmt.Sprintf("https://%s/%s", r.host, strings.ReplaceAll(r.path, " › ", "/"))
	if p.encoded {
		// The encrypted form: the destination is genuinely absent from the
		// page. The placeholder stands in for the ciphertext.
		href = fmt.Sprintf("/goto?url=ENCRYPTED%02d", i)
	}
	title := fmt.Sprintf("<h3>%s</h3>", esc(r.title))
	if p.cards {
		title = fmt.Sprintf(`<div role="heading" aria-level="3">%s</div>`, esc(r.title))
	}
	cite := ""
	if !p.cards && r.host != "" {
		cite = fmt.Sprintf(`<cite>https://%s › %s</cite>`, esc(r.host), esc(r.path))
	}
	// The snippet lives in a div that is a SIBLING of the title+cite wrapper,
	// both under the outer data-snc box — not nested inside that wrapper.
	// Measured on real pages: a climb that stops at the nearest ancestor
	// (the inner wrapper) finds the title and the cite but never reaches the
	// description at all.
	return fmt.Sprintf(
		`<div data-snc="r%d"><div><a href="%s" data-ved="x">%s</a>%s</div><div>%s</div></div>`+"\n",
		i, esc(href), title, cite, snippetHTML(r.snippet))
}

// snippetHTML renders a snippet with its first word wrapped in em, the way
// Google emphasises the query's own terms inside a real snippet. ownText
// alone cannot see through that wrapping — see snippetOf's doc comment — so a
// fixture with no em anywhere in its snippet could not catch that bug.
func snippetHTML(snippet string) string {
	words := strings.Fields(snippet)
	if len(words) == 0 {
		return esc(snippet)
	}
	return fmt.Sprintf("<em>%s</em> %s", esc(words[0]), esc(strings.Join(words[1:], " ")))
}

func renderAd(i int, a advert) string {
	// data-pcu carries the advertiser's address in plain text even when the
	// click URL is encrypted — measured, and the reason ads need no
	// resolution while organic results do. A tracker may follow after a comma.
	return fmt.Sprintf(
		`<div data-text-ad="1" data-ta-slot="0" data-ta-slot-pos="%d">`+
			`<a href="/goto?url=AD%02d" data-ved="x" data-pcu="%s,https://ad.doubleclick.net/">`+
			`<div role="heading" aria-level="3">%s</div></a><div>%s</div></div>`+"\n",
		i+1, i, esc(a.host), esc(a.title), esc(a.body))
}

func esc(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}
