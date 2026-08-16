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
// text ad, data-pcu carries an advertiser's address in plain text, data-snhf
// marks a result's header field and data-sncf its content field. Their values
// are placeholders — the real ones are session tokens and have no business in
// a public repository.
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
	// duration, when set, renders the video-card overlay: a running time drawn
	// on a thumbnail inside an aria-hidden="true" wrapper. Measured on real
	// video results, and the reason the walker skips that attribute — the
	// overlay is drawn for the eye and is not part of the description.
	duration string
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
	// sitelink, when set, renders an extra anchor BEFORE the one carrying
	// data-pcu. Measured: one bottom placement in a real capture leads with an
	// anchor that has no data-pcu, so a parser reading only the first anchor
	// records that ad with no destination.
	sitelink string
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
		{title: "Как тестировать методы REST API", host: "habr.com", path: "ru › articles", snippet: "Разбор подходов к тестированию REST API и типичных ошибок."},
		{title: "Один запрос, шестнадцать проверок", host: "software-testing.ru", path: "testing › for-beginners", snippet: "Как из одного запроса вытащить максимум проверок."},
		{title: "Работа с GET", host: "testgrow.ru", path: "lectures › lecture59", snippet: "Лекция о параметрах GET-запроса и их проверке."},
		{title: "Тестирование запроса", host: "www.politerm.com", path: "zuludoc › zb_query_test", snippet: "Документация по проверке запросов в ZuluGIS."},
		{title: "Как тестировать API в Postman", host: "gb.ru", path: "blog › postman", snippet: "Пошаговое руководство для начинающих."},
		{title: "Имеет ли смысл проверять всё", host: "qna.habr.com", path: "q › 1234567", snippet: "Обсуждение границ разумного покрытия."},
		{title: "Инструменты тестирования API", host: "testengineer.ru", path: "tools", snippet: "Обзор инструментов и их сильных сторон."},
		{title: "Проверка ответов сервера", host: "apitest.dev", path: "guides › responses", snippet: "Что именно стоит утверждать в ответе."},
		// A Google property under the encrypted form: the destination is not in
		// the page, so the cite is the only place a Google host can show up,
		// and it must be filtered the same way classifyLink filters direct and
		// redirect links — measured to differ by link form otherwise.
		{title: "Google Search Help", host: "support.google.com", path: "websearch › answer", snippet: "Справка по операторам поиска Google."},
	}
	cards := []result{
		{title: "ChatGPT начинает блокировать прямые запросы", host: "", path: "", snippet: "Новость о смене политики доступа."},
		{title: "Как проводить нагрузочное тестирование", host: "", path: "", snippet: "Практическое руководство по нагрузке."},
		{title: "Burp Suite для тестирования безопасности", host: "", path: "", snippet: "Разбор возможностей инструмента."},
		{title: "Как пользоваться SYNTX AI", host: "", path: "", snippet: "Первые шаги в интерфейсе."},
		{title: "Автотесты без боли", host: "", path: "", snippet: "О поддерживаемых автотестах."},
		{title: "Что должен уметь QA-инженер", host: "", path: "", snippet: "Список навыков и почему они нужны."},
		{title: "Пирамида тестирования", host: "", path: "", snippet: "Классическая модель и её критика."},
		{title: "Контрактное тестирование", host: "", path: "", snippet: "Зачем нужны контракты между сервисами."},
		{title: "Мутационное тестирование", host: "", path: "", snippet: "Как доказать, что тест краснеет."},
		{title: "Тест-дизайн на практике", host: "", path: "", snippet: "Техники и когда они уместны."},
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
		site = append(site, result{title: title, host: "blanktrail.com",
			path: fmt.Sprintf("page%d", i+1), snippet: "Страница сайта BlankTrail Proxy."})
	}
	us := []result{
		{title: "Used iPhones | Shop Certified Refurbished iPhones", host: "buy.gazelle.com", path: "iphone", snippet: "Certified refurbished iPhones with a warranty."},
		{title: "Shop the Newest Apple iPhones - AT&T", host: "www.att.com", path: "buy › phones", snippet: "Deals on the latest Apple iPhone models."},
		{title: "Shop Apple iPhone: Find Prices, Specs & Deals", host: "www.boostmobile.com", path: "phones › apple", snippet: "Compare iPhone prices and plans."},
		{title: "Apple Cell Phones & Smartphones", host: "www.ebay.com", path: "b › apple", snippet: "Listings for new and used Apple phones."},
		{title: "iPhone - Apple", host: "www.apple.com", path: "iphone", snippet: "The official Apple iPhone page."},
		{title: "iPhone - Wikipedia", host: "en.wikipedia.org", path: "wiki › IPhone", snippet: "Encyclopaedia article on the iPhone line."},
		{title: "New Apple iPhones & Accessories", host: "www.bestbuy.com", path: "site › apple-iphone", snippet: "Retail listings and accessories."},
		{title: "Apple iPhone: Shop Apple Smartphones", host: "www.verizon.com", path: "smartphones › apple", snippet: "Carrier offers on Apple smartphones."},
		{title: "Refurbished iPhone deals", host: "swappa.com", path: "iphone", snippet: "Marketplace listings for used iPhones."},
		{title: "Compare iPhone models", host: "www.gsmarena.com", path: "apple-phones", snippet: "Specification comparison table."},
	}
	direct := []result{
		{title: "iPhone", host: "www.apple.com", path: "iphone", snippet: "The official Apple iPhone page."},
		{title: "iPhone", host: "en.wikipedia.org", path: "wiki › IPhone", snippet: "Encyclopaedia article on the iPhone line."},
		{title: "New Apple iPhones & Accessories", host: "www.bestbuy.com", path: "site › apple-iphone", snippet: "Retail listings and accessories."},
		{title: "Apple iPhone: Shop Apple Smartphones", host: "www.verizon.com", path: "smartphones › apple", snippet: "Carrier offers on Apple smartphones."},
		{title: "Apple iPhone deals", host: "www.t-mobile.com", path: "cell-phone › apple", snippet: "Carrier promotions on iPhone."},
		{title: "Buy iPhone unlocked", host: "www.walmart.com", path: "browse › iphone", snippet: "Retail listings for unlocked phones."},
		{title: "iPhone accessories", host: "www.target.com", path: "c › iphone", snippet: "Cases, cables and chargers."},
		// A video result: the running time is drawn on the thumbnail behind
		// aria-hidden="true" and must not end up inside the description.
		{title: "Trade in your iPhone", host: "www.gazelle.com", path: "trade-in",
			snippet: "Trade-in values by model.", duration: "13:00"},
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
				{title: "Cricket Wireless® iPhone promo", host: "https://www.cricketwireless.com/", body: "Switch and save on iPhone."},
				{title: "Certified pre-owned iPhone 14", host: "https://patriotmobile.com/", body: "In stock and ready to ship."},
				{title: "iPhones, iPads, MacBooks", host: "https://www.adorama.com/", body: "Shop Apple at Adorama."},
				{title: "Apple iPhone Models", host: "https://savings.consumercellular.com/", body: "Compare models and plans."},
			},
			botAds: []advert{
				// This one leads with a sitelink that carries no data-pcu, the
				// shape that left a real bottom ad with no destination when
				// only the first anchor was read.
				{title: "Refurbished iPhones For Sale", host: "https://www.plug.tech/",
					body: "Unlocked and tested.", sitelink: "Deals under $300"},
				{title: "certified pre-owned iphone se 3", host: "https://patriotmobile.com/", body: "Devices with warranty."},
				{title: "Used & Refurbished iPhones", host: "https://reebelo.com/", body: "Free delivery, 30-day returns."},
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

	// The snippet lives in a div that is a SIBLING of the title+cite wrapper,
	// both under the outer data-snc box — not nested inside that wrapper.
	// Measured on real pages: a climb that stops at the nearest ancestor
	// (the inner wrapper) finds the title and the cite but never reaches the
	// description at all.
	overlay := ""
	if r.duration != "" {
		// A video card's thumbnail and the running time drawn on it, behind
		// aria-hidden="true" as measured. It is beside the description, not
		// part of it.
		overlay = fmt.Sprintf(`<div aria-hidden="true"><div><span>%s</span></div></div>`, esc(r.duration))
	}

	if p.cards {
		// The card layout: titles in [role=heading], no h3, no cite, and — as
		// measured — no header field and no duplicated byline either.
		title := fmt.Sprintf(`<div role="heading" aria-level="3">%s</div>`, esc(r.title))
		return fmt.Sprintf(
			`<div data-snc="r%d"><div><a href="%s" data-ved="x">%s</a></div><div>%s%s</div></div>`+"\n",
			i, esc(href), title, overlay, snippetHTML(r.snippet))
	}

	// The header field, data-snhf, holds the title, the byline and the cite —
	// and holds the byline TWICE. Measured in real markup: one copy sits
	// inside the result's own anchor, a second identical copy sits beside it
	// and is revealed by a hover animation. Both are in the DOM at all times,
	// which is why a text walker with no notion of the header field returned
	// every site name doubled.
	byline := ""
	if r.host != "" {
		byline = fmt.Sprintf(
			`<div><span aria-hidden="true"><span><img alt=""/></span></span>`+
				`<div><div><span>%s</span></div>`+
				`<div><cite>https://%s › %s</cite></div></div></div>`,
			esc(r.host), esc(r.host), esc(r.path))
	}
	return fmt.Sprintf(
		`<div data-snc="r%d"><div data-snhf="0"><div><a href="%s" data-ved="x">%s%s</a></div>%s</div>`+
			`<div data-sncf="1">%s%s</div></div>`+"\n",
		i, esc(href), fmt.Sprintf("<h3>%s</h3>", esc(r.title)), byline, byline,
		overlay, snippetHTML(r.snippet))
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
	lead := ""
	if a.sitelink != "" {
		// An anchor with no data-pcu ahead of the one that has it — measured.
		lead = fmt.Sprintf(`<a href="/goto?url=SL%02d" data-ved="x">%s</a>`, i, esc(a.sitelink))
	}

	// The byline — the advertiser's name beside its displayed address — is
	// rendered TWICE, once inside the ad's own anchor and once beside it, with
	// both copies in the DOM at all times. Measured on real pages, where a
	// walker that did not know the block opened every ad snippet with them:
	// "Apple https://www.apple.com Apple https://www.apple.com Introducing…".
	//
	// data-dtld is Google's own marker, and it sits on the address only — the
	// name beside it carries nothing. That is why the parser skips the wrapper
	// rather than the marked element, and why this fixture nests them as the
	// real page does.
	byline := fmt.Sprintf(
		`<div><span><div><span>%s</span></div>`+
			`<span data-dtld="%s" role="text">%s</span></span></div>`,
		esc(displayName(a.host)), esc(displayName(a.host)), esc(a.host))
	return fmt.Sprintf(
		`<div data-text-ad="1" data-ta-slot="0" data-ta-slot-pos="%d">%s`+
			`<a href="/goto?url=AD%02d" data-ved="x" data-pcu="%s,https://ad.doubleclick.net/">`+
			`<div role="heading" aria-level="3">%s</div>%s</a>%s<div>%s</div></div>`+"\n",
		i+1, lead, i, esc(a.host), esc(a.title), byline, byline, esc(a.body))
}

func esc(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

// displayName renders the label Google draws beside an ad's address — the
// advertiser's own name, taken here from its host so a fixture needs no extra
// field to carry it.
func displayName(rawURL string) string {
	h := strings.TrimPrefix(strings.TrimPrefix(rawURL, "https://"), "http://")
	h = strings.TrimPrefix(h, "www.")
	if i := strings.IndexAny(h, "/?"); i >= 0 {
		h = h[:i]
	}
	return h
}
