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
	function watch() {
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
		source.addEventListener("change", function () {
			if (source.value === "gateways" && Number(renew.value) === 0) {
				renew.value = "10";
			}
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
