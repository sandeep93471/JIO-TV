// Rail scroll arrows + poster fade-in for the app-style home and any page
// using .rail-shell / .media-card markup. Vanilla, no dependencies.
(function () {
  "use strict";

  function updateButtons(shell, scroller) {
    const prev = shell.querySelector(".rail-prev");
    const next = shell.querySelector(".rail-next");
    if (!prev || !next) return;
    const max = scroller.scrollWidth - scroller.clientWidth;
    prev.disabled = scroller.scrollLeft <= 8;
    next.disabled = scroller.scrollLeft >= max - 8;
    scroller.classList.toggle("rail-mask", max > 16);
  }

  function bindRail(shell) {
    const scroller = shell.querySelector(".rail-scroll");
    if (!scroller) return;
    const step = () => Math.max(200, scroller.clientWidth * 0.82);
    const prev = shell.querySelector(".rail-prev");
    const next = shell.querySelector(".rail-next");
    if (prev) {
      prev.addEventListener("click", () => {
        scroller.scrollBy({ left: -step(), behavior: "smooth" });
      });
    }
    if (next) {
      next.addEventListener("click", () => {
        scroller.scrollBy({ left: step(), behavior: "smooth" });
      });
    }
    scroller.addEventListener("scroll", () => updateButtons(shell, scroller), {
      passive: true,
    });
    window.addEventListener("resize", () => updateButtons(shell, scroller));
    updateButtons(shell, scroller);
  }

  function fadeInPosters() {
    const imgs = document.querySelectorAll(".media-card img, .hero-spot img");
    imgs.forEach((img) => {
      if (img.complete && img.naturalWidth > 0) {
        img.style.opacity = "1";
        return;
      }
      img.style.opacity = "0";
      img.style.transition = "opacity 0.35s ease, transform 0.4s ease";
      img.addEventListener(
        "load",
        () => {
          img.style.opacity = "1";
        },
        { once: true }
      );
      img.addEventListener(
        "error",
        () => {
          img.style.opacity = "1";
        },
        { once: true }
      );
    });
  }

  function init() {
    document.querySelectorAll(".rail-shell").forEach(bindRail);
    fadeInPosters();
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
