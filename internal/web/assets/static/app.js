// SPDX-License-Identifier: MIT

// Swaps one screen for another without reloading the browser tab, and asks a
// screen that is following something for itself again while it is being read.
//
// Every screen this puts on the page was drawn whole by the server at an address
// of its own. This file fetches that same address and moves what came back into
// place; it works nothing out. Which tab is lit, and whether a screen is worth
// asking for again, were settled by the server before the markup left it, which
// is where those decisions can be read and tested. What is left here is carrying
// them out.
//
// A browser that never runs this file follows the same links and is shown the
// same pages by the same server. Nothing here is loaded from anywhere.
(function () {
	// The two parts of a page that differ between one address and the next, and
	// the strip of tabs a press is taken over on. The server names all three in
	// the markup.
	var screenAt = "page";
	var headerAt = "header";
	var tabsAt = "tabs";

	// Which screen the timers below belong to. Every swap raises it, and a timer
	// begun by the screen before finds its own number stale and stops. Without
	// this, the job that was on the screen a minute ago goes on being asked after
	// and written into a page that no longer holds it.
	var showing = 0;

	// What to do when the tab is looked at again. It is one function held here
	// rather than a listener added per screen: a reader who moves between screens
	// all afternoon would otherwise collect a listener for every screen they have
	// left, each of them doing nothing, forever.
	var wake = null;

	function part(doc, name) {
		return doc.getElementById(name);
	}

	// fetched hands back the text of a page, and refuses anything that was not
	// one, so that a refusal or a wind-down is never swapped in as a screen.
	function fetched(address) {
		return fetch(address, { headers: { "Accept": "text/html" } }).then(function (answer) {
			if (!answer.ok) {
				throw new Error(answer.status);
			}
			return answer.text();
		});
	}

	// swap puts a screen that has arrived where the old one stood, header and
	// all, and says whether it recognised what arrived.
	//
	// The header travels with the screen because it is about the screen: the
	// server marked the tab in it, and the switcher in it offers this same address
	// in the other language. Working either of those out here would be a decision
	// made a second time, in the one place nothing can test it.
	function swap(html) {
		var arrived = new DOMParser().parseFromString(html, "text/html");
		var screen = part(arrived, screenAt);
		var header = part(arrived, headerAt);
		var here = part(document, screenAt);
		var lit = part(document, headerAt);
		if (!screen || !header || !here || !lit) {
			return false;
		}
		here.replaceWith(screen);
		lit.replaceWith(header);
		document.title = arrived.title;
		// The screen that just arrived has never been through anything that runs
		// once on load. What listens for this is what shapes the new-job form,
		// and without it that form works on a reload and not on a swap.
		window.dispatchEvent(new Event("gserp:screen"));
		return true;
	}

	// show fetches an address and puts the screen at it on the page.
	//
	// A press is written into the browser's history and a step back through that
	// history is not: the browser has already moved by then, and writing it down
	// again would trap the reader in their own steps.
	function show(address, remember, lost) {
		fetched(address).then(function (html) {
			if (!swap(html)) {
				throw new Error("that address answered with something else");
			}
			if (remember) {
				window.history.pushState(null, "", address);
			}
			watch();
		}).catch(lost);
	}

	// watch begins whatever the screen now on the page says to follow, and
	// abandons whatever the screen before it had begun.
	// Whether the reader is in the middle of filling something in.
	//
	// Every screen that follows something asks for itself again every few seconds
	// and puts back what the server drew — which is the whole point while a job
	// runs, and a nuisance on the same page's form: a number half typed is a
	// number the next redraw throws away, and on a three-second interval that is
	// most of them.
	//
	// So a screen with a box being filled in is left alone until it is not. Two
	// things count as filling in: something on the page has the reader's
	// attention, and something on it has been changed since it was drawn. The
	// second outlasts the first, because a reader who types a number and then
	// looks away to read the hint under it has not finished.
	//
	// It is cleared when the form is sent, which is the moment what they typed
	// stops being theirs alone and becomes what the server holds.
	var edited = false;

	function editing() {
		if (edited) {
			return true;
		}
		var here = document.activeElement;
		if (!here || !here.closest) {
			return false;
		}
		if (!here.closest("#" + screenAt)) {
			return false;
		}
		var kind = here.tagName;
		return kind === "INPUT" || kind === "SELECT" || kind === "TEXTAREA";
	}

	document.addEventListener("input", function (event) {
		if (event.target && event.target.closest && event.target.closest("#" + screenAt)) {
			edited = true;
		}
	});
	document.addEventListener("change", function (event) {
		if (event.target && event.target.closest && event.target.closest("#" + screenAt)) {
			edited = true;
		}
	});
	document.addEventListener("submit", function () {
		edited = false;
	});

	function watch() {
		edited = false;
		showing++;
		// The screen that was here may have left a way to be woken. It belongs to a
		// screen that is gone, and a screen with nothing to follow leaves none.
		wake = null;
		refresh(showing);
	}

	document.addEventListener("visibilitychange", function () {
		if (!document.hidden && wake) {
			wake();
		}
	});

	// refresh asks a screen's own address for that screen again, as often as the
	// server said to and only while it says so.
	//
	// What comes back is the whole screen, so nothing on it can be half new: there
	// is no way here to move one figure and leave the one beside it as it was an
	// hour ago. It is also how a result that has just arrived reaches the page —
	// the counts alone could never show one. A screen the server marked with no
	// interval is one where nothing can come back different until somebody presses
	// something, and it is left alone rather than asked all night.
	function refresh(mine) {
		var screen = part(document, screenAt);
		var every = screen ? Number(screen.dataset.refresh) : 0;
		if (!every) {
			return;
		}
		// A screen whose server has gone away stops asking rather than knocking
		// forever, and one answer lost on the way is not a server that has gone
		// away.
		var triesLeft = 3;
		var waiting = false;

		var ask = function () {
			waiting = false;
			if (mine !== showing) {
				return;
			}
			// A tab nobody is looking at is asked nothing. A run takes hours and a
			// browser holds a dozen tabs; a hidden one that kept fetching a page
			// every three seconds would spend an afternoon drawing screens nobody
			// sees. What it costs is that the figures are stale for as long as the
			// tab is away, which is put right the instant it comes back.
			if (document.hidden) {
				return;
			}
			// Somebody is filling something in. Come back later rather than
			// drawing over what they have typed.
			if (editing()) {
				later();
				return;
			}
			fetched(window.location.href).then(function (html) {
				if (mine === showing && swap(html)) {
					watch();
				}
			}).catch(function () {
				triesLeft--;
				if (triesLeft > 0) {
					later();
				}
			});
		};

		var later = function () {
			if (waiting || mine !== showing) {
				return;
			}
			waiting = true;
			window.setTimeout(ask, every);
		};

		// Coming back to the tab asks straight away rather than waiting out the
		// gap: somebody who has just looked at a screen is asking about now, and a
		// screen three seconds stale reads as a screen that has stopped.
		wake = function () {
			if (mine !== showing) {
				return;
			}
			triesLeft = 3;
			ask();
			later();
		};
		later();
	}

	// A press on a tab is taken over only where the browser would otherwise have
	// done the plain thing with it. A press with a key held down, or with any
	// button but the first, means open it somewhere else, and a reader who asked
	// for a new window and got their old one swapped has been overruled.
	document.addEventListener("click", function (press) {
		if (press.defaultPrevented || press.button !== 0 ||
			press.metaKey || press.ctrlKey || press.shiftKey || press.altKey) {
			return;
		}
		var strip = part(document, tabsAt);
		var link = press.target.closest ? press.target.closest("a") : null;
		if (!link || !strip || !strip.contains(link)) {
			return;
		}
		var address = link.getAttribute("href");
		press.preventDefault();
		show(address, true, function () {
			// Whatever went wrong, the browser can do this itself, and that is what
			// it would have done had this file never loaded.
			window.location.assign(address);
		});
	});

	// A form that undoes something nothing can put back asks first.
	//
	// It asks here rather than from an attribute in the markup, because a page
	// that runs anything out of its own markup is a second script nobody can read
	// as one and no test reads at all. A reader with no script is not stopped:
	// the form sends itself, the server does what it says, and a button that
	// needed a script to be safe would be a button that is not safe.
	document.addEventListener("submit", function (sending) {
		var said = sending.target.getAttribute ? sending.target.getAttribute("data-confirm") : null;
		if (said && !window.confirm(said)) {
			sending.preventDefault();
		}
	});

	// Back and forward walk the addresses already written down, and the screen has
	// to walk with them: an address bar naming one screen over a page showing
	// another is the expectation that swapping content breaks for everybody.
	window.addEventListener("popstate", function () {
		show(window.location.pathname + window.location.search, false, function () {
			window.location.reload();
		});
	});


	// The new-job form shows every box it has, and this puts away the ones the
	// choices above them have made beside the point: the site a position check
	// is about is not a question a parse answers, and a list typed in is not a
	// list in a file.
	//
	// Hiding is done here and not on the server for one reason: without a script
	// the form still has to work, and a box that only the server can put back is
	// a box a reader without a script can never fill in. So the markup carries
	// everything, this hides what does not apply, and a browser with no script
	// shows the lot — more than somebody needs, and never less.
	// closestField is the block a box stands in, which is what is shown or put
	// away — a box hidden while its label stays is a label for nothing.
	function closestField(box) {
		return box ? box.closest(".field") || box : null;
	}

	function shape(root) {
		var kind = root.querySelector("[name=kind]");
		var from = root.querySelector("[name=from]");
		if (!kind || !from) {
			return;
		}
		var forKind = {
			search: ["target"],
			position: [],
			index: ["target", "keep"]
		};
		var apply = function () {
			var hidden = forKind[kind.value] || [];
			mark(root, "target", hidden.indexOf("target") >= 0);
			mark(root, "keep", hidden.indexOf("keep") >= 0);
			// The depth is settled for an index check whatever the box says: the
			// first page answers the question, and the handler forces it.
			mark(root, "pages", kind.value === "index");
			mark(root, "queries", from.value === "file");
			mark(root, "list", from.value !== "file");
		};
		kind.addEventListener("change", apply);
		from.addEventListener("change", apply);
		apply();
	}

	// mark hides or shows the field a box stands in, along with whatever the page
	// wrote under it: a box put away without its own sentence leaves the sentence
	// explaining something nobody can see.
	function mark(root, name, away) {
		// The parts of a result are a group of boxes sharing one name rather than
		// one box, so the group itself is what is put away.
		var field = name === "keep"
			? root.querySelector(".keep-group")
			: closestField(root.querySelector("[name=" + name + "]"));
		if (!field) {
			return;
		}
		field.hidden = away;
		var said = field.nextElementSibling;
		if (said && said.classList.contains("empty")) {
			said.hidden = away;
		}
	}

	// The settings offer to look through the folders beside this program for a
	// list of addresses, which is an offer only worth making when the addresses
	// are read from a file at all. Left up, it is a link that leads somewhere the
	// reader has no use for and then back again.
	function shapeSettings(root) {
		var source = root.querySelector("[name=source]");
		var chooser = root.querySelector('a[href="/proxies/browse"]');
		if (!source || !chooser) {
			return;
		}
		// The box for where a list is read from, and the link that browses for
		// one. Neither means anything while the gateways are chosen: what is
		// behind a gateway lives in the service, and a box asking for a path
		// beside it reads as a thing left unfilled.
		//
		// Hidden rather than emptied — the value goes on travelling with the
		// form, so switching to the gateways and back finds the address where it
		// was left.
		var where = root.querySelector("[name=source_at]");
		var whereField = where && where.closest(".field");
		var apply = function () {
			chooser.hidden = source.value !== "file";
			if (whereField) {
				whereField.hidden = source.value === "gateways" || source.value === "";
			}
		};
		source.addEventListener("change", apply);
		apply();

		// Choosing the gateways offers ten minutes between changes of identity.
		//
		// A long list of addresses wants none: a port there meets a different
		// address every few requests anyway, and changing identity throws away a
		// warm one that cost minutes to make. A dozen gateways held for hours are
		// a dozen identities an origin comes to know, and what it does about that
		// is a challenge on every request.
		//
		// It is offered rather than applied: the box is filled in, in front of
		// the reader, and only when it says never — a number somebody chose is
		// theirs, and nothing here is saved until they press save.
		var renew = root.querySelector("[name=renew_minutes]");
		if (!renew) {
			return;
		}
		// Only while the box still holds what the server drew. A number the
		// reader typed is theirs, and the offer is for somebody who has not
		// thought about this box at all — which, now that the machine's own
		// default is an hour, is most people who reach for the gateways.
		var drawn = renew.value;
		source.addEventListener("change", function () {
			if (source.value === "gateways" && renew.value === drawn) {
				renew.value = "10";
			}
		});
		renew.addEventListener("input", function () {
			// Typed in, so it is no longer what was drawn and the offer is off.
			drawn = null;
		});
	}

	// Ticking two-and-thirty boxes one at a time to use a whole subscription is
	// not a choice being made, it is a chore standing in front of one. A pair of
	// buttons over the list does the whole of it, and a pair in each legend does
	// one subscription, which is the unit somebody actually bought.
	//
	// The count in the legend is rewritten as the ticks move. Left alone it goes
	// on reporting what was saved, so a reader who has just ticked everything is
	// told nought are chosen and reasonably concludes the button did nothing.
	function ticksIn(root) {
		return root ? root.querySelectorAll("input[name=gateway]") : [];
	}

	function retally(root) {
		var groups = root.querySelectorAll("fieldset[data-gateways]");
		// The header carries the whole list's tally, and a reader who has
		// scrolled away from the groups has only that to read.
		var whole = root.querySelector("[data-chosen]");
		if (whole) {
			var all = ticksIn(root);
			var ticked = 0;
			for (var k = 0; k < all.length; k++) {
				if (all[k].checked) {
					ticked++;
				}
			}
			whole.textContent = ticked + "/" + all.length;
		}
		for (var i = 0; i < groups.length; i++) {
			var tally = groups[i].querySelector("[data-tally]");
			if (!tally) {
				continue;
			}
			var boxes = ticksIn(groups[i]);
			var chosen = 0;
			for (var j = 0; j < boxes.length; j++) {
				if (boxes[j].checked) {
					chosen++;
				}
			}
			tally.textContent = chosen + "/" + boxes.length;
		}
	}

	function tick(root, on) {
		var boxes = ticksIn(root);
		for (var i = 0; i < boxes.length; i++) {
			boxes[i].checked = on;
		}
	}

	document.addEventListener("click", function (event) {
		var pressed = event.target.closest ? event.target.closest("[data-tick]") : null;
		if (!pressed) {
			return;
		}
		var within = pressed.closest("fieldset[data-gateways]") || document.querySelector(".scrolls");
		if (!within) {
			return;
		}
		tick(within, pressed.getAttribute("data-tick") === "all");
		retally(document);
	});

	document.addEventListener("change", function (event) {
		if (event.target && event.target.name === "gateway") {
			retally(document);
		}
	});


	// The walk through the interface: one stop at a time, a note beside the thing
	// it is talking about.
	//
	// Where it goes and what it says are the server's, written into the page as a
	// hidden list; everything here is carrying that out — move to the screen the
	// stop names, find the thing on it, put the note where it can be seen. A
	// phrase written in this file would be a phrase the catalogue does not hold
	// and nobody translates, so there is not one.
	var walking = -1;
	var note = null;
	var ring = null;

	function stops() {
		var box = document.getElementById("tour");
		if (!box) {
			return [];
		}
		var out = [];
		var written = box.querySelectorAll("[data-anchor]");
		for (var i = 0; i < written.length; i++) {
			out.push({
				at: written[i].getAttribute("data-at"),
				anchor: written[i].getAttribute("data-anchor"),
				title: written[i].getAttribute("data-title"),
				said: written[i].getAttribute("data-said")
			});
		}
		return out;
	}

	// walkAway ends the walk, however it ended, and says so to the server so that
	// it does not start itself again on the next screen.
	function walkAway() {
		walking = -1;
		if (note) {
			note.remove();
			note = null;
		}
		if (ring) {
			ring.remove();
			ring = null;
		}
		window.removeEventListener("resize", place);
		window.removeEventListener("scroll", place, true);
		fetch("/guide/done", { method: "POST" }).catch(function () {
			// A machine that would not write it down is a machine that offers the
			// walk again, which is a great deal better than a page that stops
			// working over it.
		});
	}

	// walkTo puts the walk on one stop: the screen first, then the note.
	function walkTo(n) {
		var all = stops();
		if (n < 0 || n >= all.length) {
			walkAway();
			return;
		}
		walking = n;
		var stop = all[n];
		if (window.location.pathname === stop.at) {
			draw(stop, n, all.length);
			return;
		}
		// The same swap a press on a tab does, so the walk moves the way the
		// program moves and the screen it lands on is the one the server drew.
		//
		// It is written into the browser's history like any other press, and it
		// has to be: the address bar is what the next stop asks where it is, and a
		// walk that moved the screen without moving the address would be told it
		// was still on the screen it left.
		show(stop.at, true, function () {
			window.location.assign(stop.at);
		});
		var once = function () {
			window.removeEventListener("gserp:screen", once);
			if (walking === n) {
				draw(stop, n, all.length);
			}
		};
		window.addEventListener("gserp:screen", once);
	}

	// draw puts the note and the ring on the screen, and hangs the presses on
	// them: one that goes on, and one that gives up.
	function draw(stop, n, total) {
		if (!note) {
			note = document.createElement("div");
			note.className = "tour";
			document.body.appendChild(note);
			window.addEventListener("resize", place);
			// In the capture phase: what scrolls is usually a box inside the page
			// rather than the page itself, and a listener on the window alone would
			// never hear it.
			window.addEventListener("scroll", place, true);
		}
		var last = n + 1 >= total;
		note.textContent = "";

		var head = document.createElement("div");
		head.className = "tour-head";
		var title = document.createElement("strong");
		title.textContent = stop.title;
		var close = document.createElement("button");
		close.type = "button";
		close.className = "tour-close";
		close.setAttribute("aria-label", document.title);
		close.textContent = "✕";
		close.addEventListener("click", walkAway);
		head.appendChild(title);
		head.appendChild(close);

		var said = document.createElement("p");
		said.className = "tour-said";
		said.textContent = stop.said;

		var foot = document.createElement("div");
		foot.className = "tour-foot";
		var count = document.createElement("span");
		count.className = "tour-count";
		count.textContent = (n + 1) + "/" + total;
		var next = document.createElement("button");
		next.type = "button";
		next.className = "tour-next";
		next.textContent = last ? "✓" : "→";
		next.addEventListener("click", function () {
			walkTo(n + 1);
		});
		foot.appendChild(count);
		foot.appendChild(next);

		note.appendChild(head);
		note.appendChild(said);
		note.appendChild(foot);
		note.dataset.anchor = stop.anchor;
		// The thing being pointed at is brought into view before the note is put
		// beside it. Without this a stop whose thing is below the fold puts the
		// note where that thing would have been — off the top of the window, with
		// no ring and nothing to look at.
		var at = document.querySelector(stop.anchor);
		if (at && at.scrollIntoView) {
			at.scrollIntoView({ block: "center", inline: "nearest" });
		}
		place();
		next.focus();
	}

	// place puts the note beside the thing this stop is about, and the ring round
	// it. A stop whose thing is not on the screen — a screen drawn without it,
	// because there is nothing to draw — puts the note in the middle and draws no
	// ring: the words are the point, and a ring round nothing is a lie about where
	// to look.
	function place() {
		if (!note) {
			return;
		}
		var at = note.dataset.anchor ? document.querySelector(note.dataset.anchor) : null;
		// A thing that is on the screen and drawn as nothing — a box the form has
		// put away because the choices above it made it beside the point — is not
		// a thing to point at. Its box is nought wide and nought high, and a ring
		// round it is a ring in the corner of the window with nothing in it.
		if (at && at.getBoundingClientRect().height === 0) {
			at = null;
		}
		if (!ring) {
			ring = document.createElement("div");
			ring.className = "tour-ring";
			document.body.appendChild(ring);
		}
		if (!at) {
			ring.hidden = true;
			note.style.left = Math.max(12, (window.innerWidth - note.offsetWidth) / 2) + "px";
			note.style.top = Math.max(12, (window.innerHeight - note.offsetHeight) / 3) + "px";
			return;
		}
		var box = at.getBoundingClientRect();
		ring.hidden = false;
		ring.style.left = (box.left - 6) + "px";
		ring.style.top = (box.top - 6) + "px";
		ring.style.width = (box.width + 12) + "px";
		ring.style.height = (box.height + 12) + "px";

		// Under the thing while there is room under it, and over it when there is
		// not. Kept inside the window either way: a note half off the edge is a
		// note with a button nobody can press.
		var gap = 14;
		var top = box.bottom + gap;
		if (top + note.offsetHeight > window.innerHeight - 12) {
			top = Math.max(12, box.top - note.offsetHeight - gap);
		}
		var left = Math.min(box.left, window.innerWidth - note.offsetWidth - 12);
		note.style.left = Math.max(12, left) + "px";
		note.style.top = top + "px";
	}

	// The press in the header starts it. It is an ordinary link to the screen the
	// program opens on, so a browser running no script goes there instead of
	// doing nothing at all.
	document.addEventListener("click", function (press) {
		var link = press.target.closest ? press.target.closest("[data-tour]") : null;
		if (!link || press.defaultPrevented || press.button !== 0) {
			return;
		}
		press.preventDefault();
		walkTo(0);
	});

	// And on a machine nobody has run anything on, it starts itself. The server
	// decides that — it is the one that knows whether this machine has ever been
	// used — and says so on the page.
	if (document.querySelector("#tour[data-tour-now]") && stops().length > 0) {
		walkTo(0);
	}

	shape(document);
	shapeSettings(document);

	// A form that arrived with a swapped screen has to be shaped as well, or it
	// is the one screen where this works only on a reload.
	window.addEventListener("gserp:screen", function () {
		shape(document);
		shapeSettings(document);
	});

	watch();
})();
