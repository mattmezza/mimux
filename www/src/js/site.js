// mimux.dev — progressive enhancement only. With JS disabled every element
// stays visible and every control still works (nav + FAQ are <details>).
(function () {
  "use strict";

  // Scroll reveal. Elements are hidden at observe time, not in the stylesheet,
  // so a no-JS visit never hides content.
  var reduced = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  var els = document.querySelectorAll("[data-reveal]");
  if (!reduced && "IntersectionObserver" in window && els.length) {
    var io = new IntersectionObserver(function (entries) {
      entries.forEach(function (e) {
        if (e.isIntersecting) {
          e.target.classList.add("in-view");
          io.unobserve(e.target);
        }
      });
    }, { rootMargin: "0px 0px -8% 0px", threshold: 0.05 });
    els.forEach(function (el, i) {
      var d = el.getAttribute("data-reveal");
      if (d) el.style.setProperty("--reveal-delay", d + "ms");
      el.classList.add("reveal-init");
      io.observe(el);
    });
  }

  // Currency switch. Prices ship as EUR in the HTML and carry their USD twin in
  // data-usd; the choice is stored so it survives a hop between pages. This is a
  // deferred script, so it runs before DOMContentLoaded — no flash of EUR.
  var prices = document.querySelectorAll("[data-usd]");
  var curBtns = document.querySelectorAll("[data-currency]");
  // The buy forms post cross-origin to the shop's /checkout, which rejects a
  // currency it does not sell and charges the one it is given. So the hidden
  // field follows the switch: without this, someone reading dollars would be
  // charged the euro price they never saw.
  var curFields = document.querySelectorAll("[data-currency-field]");
  if (prices.length || curFields.length) {
    var setCurrency = function (cur) {
      prices.forEach(function (el) {
        if (!el.getAttribute("data-eur")) el.setAttribute("data-eur", el.textContent);
        el.textContent = el.getAttribute(cur === "usd" ? "data-usd" : "data-eur");
      });
      curFields.forEach(function (f) { f.value = cur; });
      curBtns.forEach(function (b) {
        b.setAttribute("aria-pressed", b.getAttribute("data-currency") === cur ? "true" : "false");
      });
    };
    var stored = null;
    try { stored = localStorage.getItem("mimux.currency"); } catch (e) { /* private mode */ }
    if (stored === "usd") setCurrency("usd");
    curBtns.forEach(function (b) {
      b.addEventListener("click", function () {
        var cur = b.getAttribute("data-currency");
        setCurrency(cur);
        try { localStorage.setItem("mimux.currency", cur); } catch (e) { /* private mode */ }
      });
    });
  }

  // Buy buttons: the POST goes to the shop and on to Stripe, so nothing on this
  // page changes while the browser waits. Spin the button so the click is
  // acknowledged, and refuse the second one — it would open a second checkout
  // session. Coming back from Stripe restores this page from the bfcache with
  // the button still spinning, so pageshow clears it.
  var buyForms = document.querySelectorAll("form[action$='/checkout']");
  buyForms.forEach(function (form) {
    form.addEventListener("submit", function (e) {
      if (form.classList.contains("is-busy")) { e.preventDefault(); return; }
      form.classList.add("is-busy");
      var btn = form.querySelector("button[type=submit]");
      if (btn) {
        btn.classList.add("is-busy");
        btn.setAttribute("aria-busy", "true");
      }
    });
  });
  if (buyForms.length) {
    window.addEventListener("pageshow", function (e) {
      if (!e.persisted) return;
      buyForms.forEach(function (form) {
        form.classList.remove("is-busy");
        form.querySelectorAll(".is-busy").forEach(function (b) {
          b.classList.remove("is-busy");
          b.removeAttribute("aria-busy");
        });
      });
    });
  }

  // Copy-to-clipboard buttons: <button data-copy="text to copy">
  document.querySelectorAll("[data-copy]").forEach(function (btn) {
    var label = btn.textContent;
    btn.addEventListener("click", function () {
      navigator.clipboard.writeText(btn.getAttribute("data-copy")).then(function () {
        btn.textContent = "copied";
        setTimeout(function () { btn.textContent = label; }, 1600);
      });
    });
  });

  // Close the mobile nav when a link inside it is followed.
  var nav = document.querySelector(".nav-toggle");
  if (nav) {
    nav.querySelectorAll("a").forEach(function (a) {
      a.addEventListener("click", function () { nav.removeAttribute("open"); });
    });
  }
})();
