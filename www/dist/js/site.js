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
