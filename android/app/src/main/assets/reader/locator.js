/* SPDX-License-Identifier: AGPL-3.0-or-later - GENERATED from web/src/lib/reader/locator.ts by make android-reader-js. Do not edit. */
"use strict";
var Locator = (() => {
  var __defProp = Object.defineProperty;
  var __getOwnPropDesc = Object.getOwnPropertyDescriptor;
  var __getOwnPropNames = Object.getOwnPropertyNames;
  var __hasOwnProp = Object.prototype.hasOwnProperty;
  var __export = (target, all) => {
    for (var name in all)
      __defProp(target, name, { get: all[name], enumerable: true });
  };
  var __copyProps = (to, from, except, desc) => {
    if (from && typeof from === "object" || typeof from === "function") {
      for (let key of __getOwnPropNames(from))
        if (!__hasOwnProp.call(to, key) && key !== except)
          __defProp(to, key, { get: () => from[key], enumerable: !(desc = __getOwnPropDesc(from, key)) || desc.enumerable });
    }
    return to;
  };
  var __toCommonJS = (mod) => __copyProps(__defProp({}, "__esModule", { value: true }), mod);

  // src/lib/reader/locator.ts
  var locator_exports = {};
  __export(locator_exports, {
    anchorFromRange: () => anchorFromRange,
    anchorFromSelection: () => anchorFromSelection,
    elementAtPath: () => elementAtPath,
    elementPath: () => elementPath,
    firstVisibleElement: () => firstVisibleElement,
    formatEpubLocator: () => formatEpubLocator,
    normaliseRange: () => normaliseRange,
    paintHighlight: () => paintHighlight,
    parseLocator: () => parseLocator,
    pendingImages: () => pendingImages,
    rangeFromAnchor: () => rangeFromAnchor,
    unpaintHighlights: () => unpaintHighlights,
    whenLoaded: () => whenLoaded
  });
  var BLOCK = /^(P|H[1-6]|LI|BLOCKQUOTE|PRE|FIGURE|TD|DD|DT)$/;
  function elementPath(element, root) {
    const parts = [];
    let node = element;
    while (node && node !== root) {
      const parent = node.parentElement;
      if (!parent) break;
      let index = 1;
      for (const sibling of parent.children) {
        if (sibling === node) break;
        if (sibling.tagName === node.tagName) index++;
      }
      parts.unshift(`${node.tagName.toLowerCase()}[${index}]`);
      node = parent;
    }
    return parts.join("/");
  }
  function elementAtPath(path, root) {
    if (!path) return null;
    let node = root;
    for (const segment of path.split("/")) {
      const match = /^([a-z0-9-]+)\[(\d+)\]$/i.exec(segment);
      const rawTag = match == null ? void 0 : match[1];
      const rawNth = match == null ? void 0 : match[2];
      if (!rawTag || !rawNth) return null;
      const tag = rawTag.toLowerCase();
      const nth = Number(rawNth);
      let seen = 0;
      let found = null;
      for (const child of node.children) {
        if (child.tagName.toLowerCase() === tag) {
          seen++;
          if (seen === nth) {
            found = child;
            break;
          }
        }
      }
      if (!found) return null;
      node = found;
    }
    return node;
  }
  function firstVisibleElement(flow, columnLeft, columnWidth) {
    const flowBox = flow.getBoundingClientRect();
    let best = null;
    let bestOffset = Infinity;
    for (const el of flow.querySelectorAll("*")) {
      if (!BLOCK.test(el.tagName)) continue;
      const box = el.getBoundingClientRect();
      if (box.width === 0 && box.height === 0) continue;
      const left = box.left - flowBox.left;
      if (left < columnLeft - 1) continue;
      if (left > columnLeft + columnWidth) continue;
      if (left < bestOffset) {
        bestOffset = left;
        best = el;
      }
    }
    return best;
  }
  function pendingImages(root, target) {
    const out = [];
    for (const img of root.querySelectorAll("img")) {
      if (img.complete) continue;
      if (target) {
        const at = img.compareDocumentPosition(target);
        if (!(at & (Node.DOCUMENT_POSITION_FOLLOWING | Node.DOCUMENT_POSITION_CONTAINS))) continue;
      }
      out.push(img);
    }
    return out;
  }
  function whenLoaded(images, ms) {
    return new Promise((resolve) => {
      let left = images.filter((img) => !img.complete).length;
      if (left === 0) {
        resolve();
        return;
      }
      const timer = setTimeout(done, ms);
      function done() {
        clearTimeout(timer);
        for (const img of images) {
          img.removeEventListener("load", one);
          img.removeEventListener("error", one);
        }
        resolve();
      }
      function one() {
        if (--left <= 0) done();
      }
      for (const img of images) {
        if (img.complete) continue;
        img.addEventListener("load", one);
        img.addEventListener("error", one);
      }
    });
  }
  function formatEpubLocator(spineIndex, path, charOffset = 0) {
    return `epub:${spineIndex}/${path}:${charOffset}`;
  }
  function parseLocator(locator) {
    const match = /^epub:(\d+)\/(.+):(\d+)$/.exec(locator);
    const spine = match == null ? void 0 : match[1];
    const path = match == null ? void 0 : match[2];
    const offset = match == null ? void 0 : match[3];
    if (!spine || !path || offset === void 0) return null;
    return {
      format: "epub",
      spineIndex: Number(spine),
      path,
      charOffset: Number(offset)
    };
  }
  function textNodes(root) {
    const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
    const out = [];
    let node = walker.nextNode();
    while (node) {
      out.push(node);
      node = walker.nextNode();
    }
    return out;
  }
  function blockAncestor(node, root) {
    let el = node.nodeType === Node.ELEMENT_NODE ? node : node.parentElement;
    while (el && el !== root) {
      if (BLOCK.test(el.tagName)) return el;
      el = el.parentElement;
    }
    return null;
  }
  function offsetWithin(block, container, offset) {
    let count = 0;
    for (const node of textNodes(block)) {
      if (node === container) return count + offset;
      count += node.data.length;
    }
    return count;
  }
  function inBlock(node, root) {
    return blockAncestor(node, root) !== null;
  }
  function lastDescendant(node) {
    let out = node;
    while (out.lastChild) out = out.lastChild;
    return out;
  }
  function textPoint(container, offset, forward, root) {
    if (container.nodeType === Node.TEXT_NODE && inBlock(container, root)) {
      return { node: container, offset };
    }
    const usable = (node) => !!node && node.nodeType === Node.TEXT_NODE && node.data.length > 0 && inBlock(node, root);
    const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
    const isText = container.nodeType === Node.TEXT_NODE;
    if (forward) {
      let from2;
      if (isText) {
        from2 = container;
      } else {
        const child = container.childNodes[offset];
        if (child) {
          if (usable(child)) return { node: child, offset: 0 };
          from2 = child;
        } else {
          from2 = lastDescendant(container);
        }
      }
      walker.currentNode = from2;
      for (let node = walker.nextNode(); node; node = walker.nextNode()) {
        if (usable(node)) return { node, offset: 0 };
      }
      return null;
    }
    let from = container;
    if (!isText && offset > 0) {
      const child = container.childNodes[offset - 1];
      if (child) {
        const last = lastDescendant(child);
        if (usable(last)) return { node: last, offset: last.data.length };
        from = last;
      }
    }
    walker.currentNode = from;
    for (let node = walker.previousNode(); node; node = walker.previousNode()) {
      if (usable(node)) return { node, offset: node.data.length };
    }
    return null;
  }
  function normaliseRange(range, root) {
    if (!root.contains(range.startContainer) || !root.contains(range.endContainer)) return null;
    const start = textPoint(range.startContainer, range.startOffset, true, root);
    const end = textPoint(range.endContainer, range.endOffset, false, root);
    if (!start || !end) return null;
    const out = document.createRange();
    try {
      out.setStart(start.node, start.offset);
      out.setEnd(end.node, end.offset);
    } catch {
      return null;
    }
    return out.collapsed ? null : out;
  }
  function rangeText(range, root) {
    const lines = [];
    let block;
    let line = "";
    let pre = false;
    const flush = () => {
      const text = pre ? line : line.replace(/\s+/g, " ");
      if (text.trim()) lines.push(pre ? trimPreformatted(text) : text.trim());
      line = "";
    };
    const common = range.commonAncestorContainer;
    const nodes = common.nodeType === Node.TEXT_NODE ? [common] : textNodes(common);
    for (const node of nodes) {
      if (node !== common && !range.intersectsNode(node)) continue;
      const from = node === range.startContainer ? range.startOffset : 0;
      const to = node === range.endContainer ? range.endOffset : node.data.length;
      const piece = node.data.slice(from, to);
      const owner = blockAncestor(node, root);
      if (!owner && !piece.trim()) continue;
      if (owner !== block) {
        flush();
        block = owner;
        pre = !!(owner == null ? void 0 : owner.closest("pre"));
      }
      line += piece;
    }
    flush();
    return lines.join("\n");
  }
  function trimPreformatted(text) {
    let start = 0;
    while (text.charCodeAt(start) === 10) start++;
    return text.slice(start).trimEnd();
  }
  function anchorFromRange(range, root) {
    if (range.collapsed) return null;
    const normal = normaliseRange(range, root);
    if (!normal) return null;
    const startBlock = blockAncestor(normal.startContainer, root);
    const endBlock = blockAncestor(normal.endContainer, root);
    if (!startBlock || !endBlock) return null;
    const text = rangeText(normal, root);
    if (!text) return null;
    return {
      startPath: elementPath(startBlock, root),
      startOffset: offsetWithin(startBlock, normal.startContainer, normal.startOffset),
      endPath: elementPath(endBlock, root),
      endOffset: offsetWithin(endBlock, normal.endContainer, normal.endOffset),
      text
    };
  }
  function anchorFromSelection(selection, root) {
    if (selection.isCollapsed || selection.rangeCount === 0) return null;
    return anchorFromRange(selection.getRangeAt(0), root);
  }
  function rangeFromAnchor(anchor, root) {
    const startBlock = elementAtPath(anchor.startPath, root);
    const endBlock = elementAtPath(anchor.endPath, root);
    if (!startBlock || !endBlock) return null;
    const startPoint = pointAt(startBlock, anchor.startOffset);
    const endPoint = pointAt(endBlock, anchor.endOffset);
    if (!startPoint || !endPoint) return null;
    const range = document.createRange();
    try {
      range.setStart(startPoint.node, startPoint.offset);
      range.setEnd(endPoint.node, endPoint.offset);
    } catch {
      return null;
    }
    if (range.collapsed) return null;
    return range;
  }
  function pointAt(block, offset) {
    let remaining = offset;
    const nodes = textNodes(block);
    for (const node of nodes) {
      if (remaining <= node.data.length) return { node, offset: remaining };
      remaining -= node.data.length;
    }
    const last = nodes[nodes.length - 1];
    return last ? { node: last, offset: last.data.length } : null;
  }
  function paintHighlight(range, id, colour) {
    var _a;
    const nodes = [];
    const container = range.commonAncestorContainer;
    if (container.nodeType === Node.TEXT_NODE) {
      nodes.push(container);
    } else {
      const walker = document.createTreeWalker(container, NodeFilter.SHOW_TEXT, {
        acceptNode(node2) {
          return range.intersectsNode(node2) ? NodeFilter.FILTER_ACCEPT : NodeFilter.FILTER_REJECT;
        }
      });
      let node = walker.nextNode();
      while (node) {
        nodes.push(node);
        node = walker.nextNode();
      }
    }
    if (nodes.length === 0) return false;
    const startContainer = range.startContainer;
    const startOffset = range.startOffset;
    const endContainer = range.endContainer;
    const endOffset = range.endOffset;
    const plan = nodes.map((text) => ({
      text,
      from: text === startContainer ? startOffset : 0,
      to: text === endContainer ? endOffset : text.data.length
    })).filter((p) => p.to > p.from);
    let painted = false;
    for (const { text, from, to } of plan) {
      const piece = from > 0 ? text.splitText(from) : text;
      if (to - from < piece.data.length) piece.splitText(to - from);
      const mark = document.createElement("mark");
      mark.className = "sm-highlight";
      mark.dataset.annotation = id;
      if (colour) mark.dataset.colour = colour;
      (_a = piece.parentNode) == null ? void 0 : _a.insertBefore(mark, piece);
      mark.appendChild(piece);
      painted = true;
    }
    return painted;
  }
  function unpaintHighlights(root) {
    for (const mark of [...root.querySelectorAll("mark.sm-highlight")]) {
      const parent = mark.parentNode;
      if (!parent) continue;
      while (mark.firstChild) parent.insertBefore(mark.firstChild, mark);
      parent.removeChild(mark);
    }
    root.normalize();
  }
  return __toCommonJS(locator_exports);
})();
