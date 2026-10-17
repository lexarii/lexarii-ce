// SPDX-License-Identifier: AGPL-3.0-or-later

/*
 * The Android reader's page logic: columns, page turns, and where the reader is.
 *
 * A port of the layout half of web/src/routes/read/[id]/+page.svelte, kept
 * deliberately close to it - the same column arithmetic and the same choice of
 * "first visible block" - because a position saved here is opened there, and
 * the two must agree on which paragraph it names. The locator code itself is
 * not ported: `Locator` is the web's own module, compiled.
 *
 * Talks to the app through `LexariiBridge`, and only in one direction per
 * concern: the app says what to show, which marks exist and how the text
 * should look; the page says where the reader is, when they ran off either end
 * of the chapter, which mark they tapped, which marks it could not find, and
 * when the centre of the page was tapped to ask for the app's controls.
 *
 * Highlights are painted the web's way, with Locator.paintHighlight: every
 * mark is a real <mark class="sm-highlight"> around the text it covers. That is
 * what makes a tap on one findable - the event target is inside it.
 */
(function () {
  'use strict';

  var bridge = window.LexariiBridge;
  var viewport = document.getElementById('viewport');
  var flow = document.getElementById('flow');

  var spineIndex = 0;
  var spineCount = 1;
  var pageIndex = 0;
  var pageCount = 1;
  var shown = false;
  var showSeq = 0;

  // The paragraph at the top of the page on screen, kept up to date as the
  // reader moves. A resize cannot work it out afterwards: by the time the
  // event fires, the text has already reflowed at the new width under the old
  // column arithmetic, and asking then names a paragraph pages away.
  var here = null;

  // How the text looks, as last applied. The theme starts as whatever the app
  // baked into the page as {{THEME}}, so the first frame is already right.
  var THEMES = ['light', 'sepia', 'dark', 'black'];
  var FONTS = ['serif', 'sans'];
  var MARGINS = ['narrow', 'normal', 'wide'];
  var typography = {
    theme: oneOf(document.documentElement.getAttribute('data-theme'), THEMES, 'light'),
    fontFamily: 'serif',
    fontScale: 1,
    lineHeight: 1.6,
    margin: 'normal',
  };

  // Every mark in the book, as the app last sent them. Only this chapter's are
  // painted, but all are kept: the app sends them once, not once per chapter.
  var marks = [];
  // The unresolved report last sent for the chapter on screen, as
  // "spineIndex:json", or null when this chapter has not reported yet. Reset by
  // every show(), so each chapter reports once when it is shown - an empty
  // list included, which is how the app learns that a mark it had given up
  // on has been found again - and then only when a repaint changes the list.
  var unresolvedSent = null;
  // A repaint that was put off because the reader was holding a selection.
  var paintPending = false;

  /** Twice the viewport padding, so no sliver of the next page shows in the margin. */
  function columnGap() {
    var padding = parseFloat(getComputedStyle(viewport).paddingLeft) || 22;
    return Math.max(44, padding * 2);
  }

  /**
   * The width of one page of text, unrounded.
   *
   * Not clientWidth, which is rounded to a whole pixel while the columns are
   * laid out at the real width. A phone's CSS width is often fractional -
   * 411.43px is common - and the difference, a fraction of a pixel, is added
   * again on every page: by the tenth the strip sits pixels off, text creeps
   * into the margin, and firstVisibleElement's one-pixel tolerance finds no
   * block on the page at all, so the position is never reported. A transform
   * is only ever a translation here, which leaves the width alone.
   */
  function pageWidth() {
    return flow.getBoundingClientRect().width;
  }

  function advance() {
    return pageWidth() + columnGap();
  }

  /** Lays the chapter out as columns and counts them. */
  function measure() {
    var width = pageWidth();
    if (width <= 0) return;
    var gap = columnGap();
    flow.style.columnWidth = width + 'px';
    flow.style.columnGap = gap + 'px';
    // scrollWidth counts n columns and n-1 gaps.
    pageCount = Math.max(1, Math.round((flow.scrollWidth + gap) / (width + gap)));
    if (pageIndex > pageCount - 1) pageIndex = pageCount - 1;
  }

  /** Moves the column strip. Only a single page turn animates; everything else jumps. */
  function applyOffset(animate) {
    var offset = 'translateX(' + (-pageIndex * advance()) + 'px)';
    if (animate) {
      flow.style.transform = offset;
      return;
    }
    var previous = flow.style.transition;
    flow.style.transition = 'none';
    flow.style.transform = offset;
    void flow.offsetWidth;
    flow.style.transition = previous;
  }

  /** The page an element falls on, at the current layout. */
  function pageOf(element) {
    var left = element.getBoundingClientRect().left - flow.getBoundingClientRect().left;
    return Math.max(0, Math.min(pageCount - 1, Math.round(left / advance())));
  }

  /** The element path of the first block on screen, or null on an empty page. */
  function visiblePath() {
    var target = Locator.firstVisibleElement(flow, pageIndex * advance(), pageWidth());
    if (!target) return null;
    return Locator.elementPath(target, flow) || null;
  }

  // Debounced like the web reader's save: flicking through ten pages is one
  // position, not ten requests.
  var reportTimer = 0;
  function scheduleReport() {
    here = visiblePath();
    clearTimeout(reportTimer);
    reportTimer = setTimeout(report, 1200);
  }

  function report() {
    clearTimeout(reportTimer);
    if (!shown) return;
    var path = visiblePath();
    if (!path) return;
    var overall = (spineIndex + (pageCount > 1 ? pageIndex / (pageCount - 1) : 0)) / spineCount;
    bridge.position(Locator.formatEpubLocator(spineIndex, path), overall);
  }

  function turn(delta) {
    if (!shown) return;
    var next = pageIndex + delta;
    if (next < 0 || next > pageCount - 1) {
      // Off the end of the chapter. The app owns the spine, so it decides
      // whether there is a next one; the page only says which way.
      report();
      bridge.edge(delta > 0 ? 1 : -1);
      return;
    }
    pageIndex = next;
    applyOffset(true);
    scheduleReport();
  }

  /** Whether the reader is holding a text selection. */
  function selecting() {
    var selection = window.getSelection();
    return !!selection && !selection.isCollapsed;
  }

  /**
   * Something on the page on screen that painting will not remove, and the
   * page it starts on - so that after a repaint the reader can be put back in
   * front of the same text.
   *
   * The first block on the page when there is one: that is `here`, the
   * paragraph a position names. Inside one long paragraph no block starts on
   * the page, so it falls back to whatever is under the top of the text.
   */
  function pageAnchor() {
    var element = Locator.firstVisibleElement(flow, pageIndex * advance(), pageWidth());
    if (!element) {
      var box = viewport.getBoundingClientRect();
      var style = getComputedStyle(viewport);
      element = document.elementFromPoint(
        box.left + (parseFloat(style.paddingLeft) || 0) + 2,
        box.top + (parseFloat(style.paddingTop) || 0) + 2
      );
      // A painted mark is about to be taken out of the document.
      while (element && element !== flow && element.matches('mark.sm-highlight')) {
        element = element.parentElement;
      }
      if (!element || element === flow || !flow.contains(element)) return null;
    }
    return { element: element, page: pageOf(element) };
  }

  /**
   * Draws this chapter's marks, from clean text every time.
   *
   * `keepPage` is for a repaint of the chapter already being read. Wrapping
   * text can move line breaks, and a moved line break can move text across a
   * page, so the page is re-found from what was on it rather than kept by
   * number. Callers that are about to choose a page themselves - show, and a
   * resize - pass false and choose it after the paint.
   */
  function paintMarks(keepPage) {
    paintPending = false;
    var anchor = keepPage ? pageAnchor() : null;

    Locator.unpaintHighlights(flow);

    var missing = [];
    for (var i = 0; i < marks.length; i++) {
      var mark = marks[i];
      if (!mark || typeof mark.id !== 'string') continue;
      var start = Locator.parseLocator(String(mark.start || ''));
      // A start that does not parse cannot be said to belong to any chapter,
      // so it is not this chapter's to report.
      if (!start || start.spineIndex !== spineIndex) continue;
      var end = Locator.parseLocator(String(mark.end || ''));
      // An end in another chapter cannot be drawn from this one, and this
      // reader never makes such a mark; it is reported, not guessed at.
      var range = end && end.spineIndex === spineIndex
        ? Locator.rangeFromAnchor({
          startPath: start.path,
          startOffset: start.charOffset,
          endPath: end.path,
          endOffset: end.charOffset,
          text: mark.selectedText || '',
        }, flow)
        : null;
      if (!range || !Locator.paintHighlight(range, mark.id, mark.colour || '')) {
        missing.push(mark.id);
        continue;
      }
      dress(mark);
    }

    // The same measure-after-paint the web does after paintMarks.
    measure();
    if (anchor) {
      pageIndex = Math.max(0, Math.min(pageCount - 1, pageIndex + pageOf(anchor.element) - anchor.page));
      applyOffset(false);
    }

    reportUnresolved(missing);
  }

  /** Marks the pieces of a painted mark with what the CSS needs to tell them apart. */
  function dress(mark) {
    if (!mark.shared && !mark.hasNote) return;
    var pieces = flow.querySelectorAll('mark.sm-highlight[data-annotation="' + CSS.escape(mark.id) + '"]');
    if (pieces.length === 0) return;
    if (mark.shared) {
      for (var i = 0; i < pieces.length; i++) pieces[i].dataset.shared = '';
    }
    // One marker at the end of the passage, not one per text node.
    if (mark.hasNote) pieces[pieces.length - 1].dataset.note = '';
  }

  /**
   * Tells the app which of this chapter's marks could not be painted, and
   * which chapter that is about: a report can reach the app after it has
   * moved on, and must not be judged against the next chapter's marks.
   *
   * Sent on the first paint after a show(), even when empty, and after that
   * only when the list changes - a repaint that finds the same marks missing
   * says nothing new.
   */
  function reportUnresolved(ids) {
    var json = JSON.stringify(ids);
    var key = spineIndex + ':' + json;
    if (key === unresolvedSent) return;
    unresolvedSent = key;
    bridge.unresolved(json, spineIndex);
  }

  /**
   * Replaces the set of marks.
   *
   * Put off while the reader holds a selection: repainting rewrites the text
   * nodes the selection is made of, and a sync arriving in the middle of a
   * long-press would otherwise yank the handles out from under a thumb.
   */
  function setMarks(list) {
    marks = Array.isArray(list) ? list.slice() : [];
    if (!shown) return; // show() paints once the chapter has laid out
    if (selecting()) {
      paintPending = true;
      return;
    }
    paintMarks(true);
  }

  /**
   * The reader's selection as a mark-to-be: {start, end, selectedText} as
   * locators, or null when there is nothing in the chapter to anchor to.
   * Returned rather than sent, because the app asked for it and reads the
   * answer.
   *
   * A selection's ends are wherever the browser put them - an element
   * boundary for a whole paragraph, the whitespace between two blocks - and
   * Locator.anchorFromSelection moves them onto nameable text itself, the
   * same way for this reader and the web's.
   *
   * Cleared only when it became a mark. A selection that could not be used is
   * left exactly as it was, so the person can adjust it rather than lose it.
   */
  function highlightSelection() {
    var selection = window.getSelection();
    if (!selection || !shown || selection.rangeCount === 0 || selection.isCollapsed) return null;
    var anchor = Locator.anchorFromSelection(selection, flow);
    if (!anchor) return null;
    selection.removeAllRanges();
    return {
      start: Locator.formatEpubLocator(spineIndex, anchor.startPath, anchor.startOffset),
      end: Locator.formatEpubLocator(spineIndex, anchor.endPath, anchor.endOffset),
      selectedText: anchor.text,
    };
  }

  /**
   * Shows one chapter.
   *
   * `target` says where in it: {path} for a saved position or a citation,
   * {fragment} for a link into it, {end: true} when turning back into it from
   * the chapter after. Anything that does not resolve opens the chapter at its
   * start: the chapter is still the right chapter, and guessing a page would
   * not be honest.
   *
   * A target is turned to once the pictures that can move it have arrived,
   * or after PICTURE_WAIT_MS; until then the page takes no turns.
   */
  function show(html, index, count, target) {
    shown = false;
    clearTimeout(reportTimer);
    clearTimeout(resizeTimer);
    clearTimeout(pictureTimer);
    spineIndex = index;
    spineCount = Math.max(1, count);
    // Sanitised on the server: bluemonday, no scripts, no handlers, no styles.
    // The page's policy would refuse a script even if one got through.
    flow.innerHTML = html;
    pageIndex = 0;
    paintPending = false;
    var seq = ++showSeq;

    // Two frames: the first lets the new content lay out, the second measures it.
    requestAnimationFrame(function () {
      requestAnimationFrame(function () {
        // A later show() replaced the chapter; its own frames will lay it out.
        if (seq !== showSeq) return;
        // What the reader was sent to, when it was something in particular.
        // Found by structure, which no picture can change.
        var element = null;
        if (target && target.path) {
          element = Locator.elementAtPath(target.path, flow);
        } else if (target && target.fragment) {
          var anchor = document.getElementById(target.fragment);
          if (anchor && flow.contains(anchor)) element = anchor;
        }
        // The pictures that can still move it - see Locator.pendingImages.
        // The start of a chapter is page one whatever arrives, so opening
        // there waits for nothing.
        var waiting = element ? Locator.pendingImages(flow, element)
          : target && target.end ? Locator.pendingImages(flow, null) : [];
        if (waiting.length === 0) {
          land(target, element);
          return;
        }
        Locator.whenLoaded(waiting, PICTURE_WAIT_MS).then(function () {
          if (seq === showSeq) land(target, element);
        });
      });
    });
  }

  /** Lays the chapter show() put up out, and turns to `element` or the end. */
  function land(target, element) {
    pageIndex = 0;
    // This chapter reports what it could not place, whatever the last
    // one said.
    unresolvedSent = null;
    // Painted before the page is chosen, so the page is chosen from the
    // layout the reader will actually see. paintMarks measures.
    paintMarks(false);
    if (element) {
      pageIndex = pageOf(element);
      if (target.cited) {
        element.classList.add('cited');
        setTimeout(function () { element.classList.remove('cited'); }, 2600);
      }
    } else if (target && target.end) {
      pageIndex = pageCount - 1;
    }
    applyOffset(false);
    shown = true;
    scheduleReport();
    // The target, rather than whichever block starts its page, is what a
    // relayout keeps on screen: a cited paragraph halfway down stays in
    // view when the type grows, not merely the one above it.
    if (element) here = Locator.elementPath(element, flow) || here;
  }

  /**
   * Lays the chapter out again after the space or the type changed, and puts
   * the paragraph being read back on screen.
   */
  function relayout() {
    // A resize debounced across a show(): the new chapter lays itself out,
    // and reports, in its own frames.
    if (!shown) return;
    var path = here;
    // Repainted unless somebody is mid-selection, which a repaint would
    // break; the marks are already in the text and reflow with it.
    if (selecting()) {
      measure();
      paintPending = true;
    } else {
      paintMarks(false);
    }
    var element = path && Locator.elementAtPath(path, flow);
    pageIndex = element ? pageOf(element) : Math.min(pageIndex, pageCount - 1);
    applyOffset(false);
    // Reported again: the same paragraph, but the page it is on - and so
    // the percentage - has moved.
    scheduleReport();
    // The paragraph being read stays the anchor, not whichever one happens
    // to start the new page: otherwise every rotation would step back a
    // paragraph or two, and a few of them would lose a page.
    if (element) here = path;
  }

  // Relayout on rotation or a window resize, keeping the paragraph being read.
  var resizeTimer = 0;
  window.addEventListener('resize', function () {
    if (!shown) return;
    clearTimeout(resizeTimer);
    resizeTimer = setTimeout(relayout, 100);
  });

  // A picture that arrives after the chapter was laid out - slower than
  // PICTURE_WAIT_MS, or below where the reader landed, or anywhere in a
  // chapter opened at its start - pushes everything after it along. Laid out
  // again around the paragraph being read, as for a rotation, once a burst of
  // arrivals has settled. A picture that fails changes the layout too: its
  // alt text takes the space instead. Neither event bubbles, so these listen
  // on the way down.
  var PICTURE_WAIT_MS = 3000;
  var pictureTimer = 0;
  function pictureArrived(event) {
    if (!shown || !event.target || event.target.tagName !== 'IMG') return;
    clearTimeout(pictureTimer);
    pictureTimer = setTimeout(relayout, 60);
  }
  flow.addEventListener('load', pictureArrived, true);
  flow.addEventListener('error', pictureArrived, true);

  /** `value` when it is one of `allowed`, otherwise `fallback`. */
  function oneOf(value, allowed, fallback) {
    return typeof value === 'string' && allowed.indexOf(value) >= 0 ? value : fallback;
  }

  /** `value` held to [lo, hi] when it is a real number, otherwise `fallback`. */
  function within(value, lo, hi, fallback) {
    if (typeof value !== 'number' || !isFinite(value)) return fallback;
    return Math.min(hi, Math.max(lo, value));
  }

  function applyTypography() {
    var root = document.documentElement;
    root.setAttribute('data-theme', typography.theme);
    root.setAttribute('data-font', typography.fontFamily);
    root.setAttribute('data-margin', typography.margin);
    // CSSOM, not a style attribute: the page's policy refuses the latter.
    root.style.setProperty('--font-scale', String(typography.fontScale));
    root.style.setProperty('--line-height', String(typography.lineHeight));
  }

  /**
   * How the text looks: {theme, fontFamily, fontScale, lineHeight, margin}.
   *
   * theme is light, sepia, dark or black; fontFamily serif or sans; fontScale
   * multiplies the default size and is held to 0.8-1.6; lineHeight is held to
   * 1.3-1.9; margin is narrow, normal or wide. Each field is checked on its
   * own, and one that is missing, of the wrong type or not in the list keeps
   * its current value - it comes from the app, but it is still input.
   *
   * Callable before the first show(), which lays out with it, and any time
   * after. A change that moves the text relays the chapter out, keeps the
   * reader on the paragraph they were reading (`here`), and reports the
   * position at once, since its page and percentage have moved.
   *
   * Returns the settings now in force, after checking.
   */
  function setTypography(settings) {
    var s = settings && typeof settings === 'object' ? settings : {};
    var next = {
      theme: oneOf(s.theme, THEMES, typography.theme),
      fontFamily: oneOf(s.fontFamily, FONTS, typography.fontFamily),
      fontScale: within(s.fontScale, 0.8, 1.6, typography.fontScale),
      lineHeight: within(s.lineHeight, 1.3, 1.9, typography.lineHeight),
      margin: oneOf(s.margin, MARGINS, typography.margin),
    };
    var reflows = next.fontFamily !== typography.fontFamily ||
      next.fontScale !== typography.fontScale ||
      next.lineHeight !== typography.lineHeight ||
      next.margin !== typography.margin;
    var changed = reflows || next.theme !== typography.theme;
    typography = next;
    if (changed) applyTypography();
    // A theme is colour only: nothing moves, so nothing is re-measured.
    if (reflows && shown) {
      clearTimeout(resizeTimer);
      relayout();
      report();
    }
    return {
      theme: typography.theme,
      fontFamily: typography.fontFamily,
      fontScale: typography.fontScale,
      lineHeight: typography.lineHeight,
      margin: typography.margin,
    };
  }

  /**
   * Links. The server rewrote every link inside the book to `?spine=N&frag=X`
   * and stripped the href from every link out of it, so there are only two
   * kinds, and neither is allowed to navigate this page.
   */
  function follow(link) {
    var href = link.getAttribute('href') || '';
    var match = /^\?spine=(\d+)(?:&frag=([^&]*))?/.exec(href);
    if (!match) return;
    var fragment = match[2] ? decodeURIComponent(match[2]) : '';
    var spine = Number(match[1]);
    if (spine === spineIndex && fragment) {
      var anchor = document.getElementById(fragment);
      if (anchor && flow.contains(anchor)) {
        pageIndex = pageOf(anchor);
        applyOffset(false);
        scheduleReport();
        return;
      }
    }
    report();
    bridge.follow(spine, fragment);
  }

  // Taps: the left third turns back, the right third forward, and the centre
  // asks the app for its controls - the text is the interface, and the
  // controls come back only when asked for. A tap that was really the end of
  // a swipe is not a tap.
  //
  // A selection takes turning away entirely, for the whole gesture. Long-press,
  // then dragging a handle or a finger to extend it, is a horizontal drag like
  // any swipe; and a tap while a selection shows is the reader dismissing it,
  // not asking for the next page. Whether there was one is decided at
  // pointerdown, before the browser's own tap handling has cleared it.
  var touchX = null;
  var touchY = null;
  var swiped = false;
  var selectedAtDown = false;
  var selectedDuring = false;
  document.addEventListener('pointerdown', function () {
    // Reset per gesture for every kind of pointer, not only a finger: a
    // swipe that ended without a click must not eat the next mouse or stylus
    // click.
    swiped = false;
    selectedAtDown = selecting();
    selectedDuring = false;
  }, { capture: true, passive: true });
  document.addEventListener('selectionchange', function () {
    if (selecting()) {
      selectedDuring = true;
    } else if (paintPending) {
      paintMarks(true);
    }
  });
  function selectionGesture() {
    return selectedAtDown || selectedDuring || selecting();
  }
  document.addEventListener('touchstart', function (event) {
    swiped = false;
    // A second finger is a pinch or a stray thumb, never a page turn.
    if (event.touches.length !== 1) {
      touchX = null;
      return;
    }
    var t = event.touches[0];
    touchX = t.clientX;
    touchY = t.clientY;
  }, { passive: true });
  document.addEventListener('touchcancel', function () {
    touchX = null;
  }, { passive: true });
  document.addEventListener('touchend', function (event) {
    if (touchX === null) return;
    var t = event.changedTouches[0];
    var dx = t.clientX - touchX;
    var dy = t.clientY - touchY;
    touchX = null;
    if (selectionGesture()) return;
    if (Math.abs(dx) > 48 && Math.abs(dx) > Math.abs(dy) * 1.5) {
      swiped = true;
      turn(dx < 0 ? 1 : -1);
    }
  }, { passive: true });

  document.addEventListener('click', function (event) {
    var target = event.target;
    var link = target.closest && target.closest('a');
    // Never allowed to navigate the page, whatever else this click is.
    if (link) event.preventDefault();
    if (swiped) {
      swiped = false;
      return;
    }
    if (selectedAtDown) {
      // Dismissing a selection, and nothing else.
      var selection = window.getSelection();
      if (selection) selection.removeAllRanges();
      return;
    }
    // Made during this gesture (a mouse drag): somebody reading closely.
    if (selectedDuring || selecting()) return;
    if (link) {
      follow(link);
      return;
    }
    // A tap on a highlight opens it - the reader's own to edit, a shared one
    // to read - instead of turning past it.
    var mark = target.closest && target.closest('mark.sm-highlight');
    if (mark && flow.contains(mark) && mark.dataset.annotation) {
      if (typeof bridge.markTapped === 'function') bridge.markTapped(mark.dataset.annotation);
      return;
    }
    var third = window.innerWidth / 3;
    if (event.clientX < third) {
      turn(-1);
    } else if (event.clientX >= third * 2) {
      turn(1);
    } else if (typeof bridge.chrome === 'function') {
      bridge.chrome();
    } else {
      // An app from before the centre meant anything: forward, as it was.
      turn(1);
    }
  });

  // A keyboard - a Chromebook, a tablet's cover - turns pages the way it
  // would in any reader. Nothing else here takes keys, and the app sends the
  // page none of its own, so there is nothing for these to collide with.
  document.addEventListener('keydown', function (event) {
    // Shift+arrow extends a selection; the modifiers belong to the system.
    if (event.altKey || event.ctrlKey || event.metaKey || event.shiftKey) return;
    var delta = event.key === 'ArrowRight' || event.key === 'PageDown' ? 1
      : event.key === 'ArrowLeft' || event.key === 'PageUp' ? -1 : 0;
    if (!delta) return;
    event.preventDefault();
    turn(delta);
  });

  window.Reader = {
    show: show,
    turn: turn,
    /** Called when the reader closes or the app leaves the screen: send the position now. */
    flush: report,
    /** Every mark in the book: {id, start, end, colour, selectedText, shared, hasNote}. */
    setMarks: setMarks,
    /** The selection as {start, end, selectedText}, or null. Clears it. */
    highlightSelection: highlightSelection,
    /** {theme, fontFamily, fontScale, lineHeight, margin}; returns what was applied. */
    setTypography: setTypography,
  };

  bridge.ready();
})();
