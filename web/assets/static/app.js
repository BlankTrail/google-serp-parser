// SPDX-License-Identifier: MIT

// Swaps one screen for another without reloading the browser tab, and keeps the
// counts of a running job up to date.
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
		follow(showing);
		refresh(showing);
	}

	// refresh asks a screen's own address for that screen again, as often as the
	// server said to and only while it says so.
	//
	// What comes back is the whole screen, so nothing on it can be half new: there
	// is no way here to move one figure and leave the one beside it as it was an
	// hour ago. A screen the server marked with no interval is one where nothing
	// can come back different until somebody presses something, and it is left
	// alone rather than asked all night.
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
		var ask = function () {
			if (mine !== showing) {
				return;
			}
			fetched(window.location.href).then(function (html) {
				if (mine === showing && swap(html)) {
					watch();
				}
			}).catch(function () {
				triesLeft--;
				if (triesLeft > 0) {
					window.setTimeout(ask, every);
				}
			});
		};
		window.setTimeout(ask, every);
	}

	// follow keeps the counts of a running job up to date without redrawing the
	// screen around them.
	//
	// Everything it writes is already on the page: the server drew each number
	// before this ran, so a browser that never runs this file shows the job as it
	// stood when the page was fetched. The markup says whether there is anything
	// to wait for; a job nobody is running will read the same in the morning, and
	// asking every two seconds until then is knocking on a door with nobody
	// behind it.
	function follow(mine) {
		var box = document.getElementById("progress");
		if (!box || !box.dataset.poll) {
			return;
		}
		var job = box.dataset.job;
		var every = Number(box.dataset.poll);
		var triesLeft = 3;

		var write = function (cell, value) {
			var at = document.getElementById(cell);
			if (at) {
				at.textContent = value;
			}
		};

		// The bar is filled by the numbers the server just sent, not by counting
		// anything here. It is absent on a job with no queries behind it, which is
		// why it is looked up rather than assumed.
		var fill = function (done, total) {
			var bar = document.getElementById("bar");
			if (bar) {
				bar.max = total;
				bar.value = done;
			}
		};

		var ask = function () {
			if (mine !== showing) {
				return;
			}
			fetch("/api/progress?job=" + encodeURIComponent(job), {
				headers: { "Accept": "application/json" }
			}).then(function (answer) {
				if (!answer.ok) {
					throw new Error(answer.status);
				}
				return answer.json();
			}).then(function (at) {
				if (mine !== showing) {
					return;
				}
				write("count-total", at.total);
				write("count-done", at.done);
				write("count-failed", at.failed);
				write("count-pending", at.pending);
				// The figure for dropped repeats is drawn only for a job that has a
				// filter, and write leaves alone what is not on the page.
				write("count-dropped", at.dropped);
				fill(at.done, at.total);
				triesLeft = 3;
				if (at.watch) {
					window.setTimeout(ask, every);
					return;
				}
				// The job has stopped, so what changed is not only the counts: the
				// results are there to be read and the buttons are not the ones drawn
				// before. The page the server draws is the answer to all of that.
				window.location.reload();
			}).catch(function () {
				triesLeft--;
				if (triesLeft > 0) {
					window.setTimeout(ask, every);
				}
			});
		};

		window.setTimeout(ask, every);
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

	// Back and forward walk the addresses already written down, and the screen has
	// to walk with them: an address bar naming one screen over a page showing
	// another is the expectation that swapping content breaks for everybody.
	window.addEventListener("popstate", function () {
		show(window.location.pathname + window.location.search, false, function () {
			window.location.reload();
		});
	});

	watch();
})();
