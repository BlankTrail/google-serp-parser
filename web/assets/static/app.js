// SPDX-License-Identifier: MIT

// Keeps the counts of a running job up to date without a reload.
//
// Everything it writes is already on the page: the server drew each number
// before this ran, so a browser that never runs this file shows the job as it
// stood when the page was fetched. Nothing here is loaded from anywhere.
(function () {
	var box = document.getElementById("progress");
	// The page says whether there is anything to wait for. A job nobody is
	// running will read the same in the morning, and asking it every two seconds
	// until then is knocking on a door with nobody behind it.
	if (!box || !box.dataset.poll) {
		return;
	}
	var job = box.dataset.job;
	var every = Number(box.dataset.poll);
	// A page whose server has gone away stops asking rather than knocking
	// forever, and one answer lost on the way is not a server that has gone away.
	var missesLeft = 3;

	function show(cell, value) {
		var at = document.getElementById(cell);
		if (at) {
			at.textContent = value;
		}
	}

	function ask() {
		fetch("/api/progress?job=" + encodeURIComponent(job), {
			headers: { "Accept": "application/json" }
		}).then(function (answer) {
			if (!answer.ok) {
				throw new Error(answer.status);
			}
			return answer.json();
		}).then(function (at) {
			show("count-total", at.total);
			show("count-done", at.done);
			show("count-failed", at.failed);
			show("count-pending", at.pending);
			missesLeft = 3;
			if (at.watch) {
				setTimeout(ask, every);
				return;
			}
			// The job has stopped, so what changed is not only the counts: the
			// results are there to be read and the buttons are not the ones drawn
			// before. The page the server draws is the answer to all of that.
			window.location.reload();
		}).catch(function () {
			missesLeft--;
			if (missesLeft > 0) {
				setTimeout(ask, every);
			}
		});
	}

	setTimeout(ask, every);
})();
