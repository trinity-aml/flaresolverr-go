package browser

import "fmt"

// The page-side scripts shared by the CDP and WebDriver backends.
//
// Both engines evaluate byte-identical JavaScript here; only the Go call that
// ships it differs (chromedp.Evaluate vs. POST /session/:id/execute/sync). They
// used to live as separate string literals in each backend, which is how the
// click heuristics drift apart. Every walker below deliberately follows
// el.fakeShadowRoot before el.shadowRoot: Cloudflare renders Turnstile inside a
// *closed* shadow root, which only a stealth Chromium build exposes, and then
// only under --enable-blink-features=FakeShadowRoot.

// ClickTargetsScript collects every plausibly clickable element on the page,
// descending through open and fake shadow roots. It returns an array that
// unmarshals into []ClickTarget.
const ClickTargetsScript = `(() => {
	const results = [];
	const visited = new Set();

	const pushTarget = (el, kind) => {
		if (!el || !el.getBoundingClientRect) return;
		const rect = el.getBoundingClientRect();
		const style = window.getComputedStyle(el);
		const text = kind === 'input' ? (el.value || '') : (el.innerText || el.textContent || '');
		const tag = (el.tagName || '').toLowerCase();
		results.push({
			kind,
			tag,
			type: el.getAttribute ? (el.getAttribute('type') || '') : '',
			text: (text || '').trim(),
			id: el.id || '',
			name: el.getAttribute ? (el.getAttribute('name') || '') : '',
			title: el.getAttribute ? (el.getAttribute('title') || '') : '',
			ariaLabel: el.getAttribute ? (el.getAttribute('aria-label') || '') : '',
			role: el.getAttribute ? (el.getAttribute('role') || '') : '',
			className: typeof el.className === 'string' ? el.className : '',
			tabIndex: typeof el.tabIndex === 'number' ? el.tabIndex : -1,
			disabled: !!el.disabled || (el.getAttribute && el.getAttribute('aria-disabled') === 'true'),
			src: kind === 'iframe' ? (el.src || (el.getAttribute && el.getAttribute('src')) || '') : '',
			left: rect.left,
			top: rect.top,
			width: rect.width,
			height: rect.height,
			visible: rect.width > 0 && rect.height > 0 && style.display !== 'none' && style.visibility !== 'hidden' && style.opacity !== '0'
		});
	};

	const walk = (root) => {
		if (!root || visited.has(root) || !root.querySelectorAll) return;
		visited.add(root);
		for (const el of root.querySelectorAll('*')) {
			const sr = el.fakeShadowRoot || el.shadowRoot;
			if (sr) walk(sr);
			const tag = (el.tagName || '').toLowerCase();
			if (tag === 'iframe' || tag === 'button' || tag === 'input' || tag === 'textarea' || tag === 'select') {
				pushTarget(el, tag);
				continue;
			}
			if (tag === 'a' && el.href) {
				pushTarget(el, 'anchor');
				continue;
			}
			if (typeof el.tabIndex === 'number' && el.tabIndex >= 0) {
				pushTarget(el, 'tabindex');
				continue;
			}
			if (el.getAttribute && el.getAttribute('role') === 'button') {
				pushTarget(el, 'role_button');
			}
		}
	};

	walk(document);
	return results;
})()`

// focusTargetScriptTemplate re-locates a previously collected ClickTarget by
// tag, attributes and geometry, then focuses it. Geometry is matched with a 2px
// tolerance because the page can reflow between the collect and the focus.
const focusTargetScriptTemplate = `(() => {
	const want = {
		tag: %q,
		id: %q,
		name: %q,
		title: %q,
		role: %q,
		ariaLabel: %q,
		className: %q,
		left: %f,
		top: %f,
		width: %f,
		height: %f
	};

	const visible = (el) => {
		if (!el || !el.getBoundingClientRect) return false;
		const rect = el.getBoundingClientRect();
		const style = window.getComputedStyle(el);
		return rect.width > 0 && rect.height > 0 && style.display !== 'none' && style.visibility !== 'hidden' && style.opacity !== '0';
	};

	const matches = (el) => {
		if (!el || !el.getBoundingClientRect) return false;
		const rect = el.getBoundingClientRect();
		if ((el.tagName || '').toLowerCase() !== want.tag) return false;
		if (want.id && el.id !== want.id) return false;
		if (want.name && (el.getAttribute('name') || '') !== want.name) return false;
		if (want.title && (el.getAttribute('title') || '') !== want.title) return false;
		if (want.role && (el.getAttribute('role') || '') !== want.role) return false;
		if (want.ariaLabel && (el.getAttribute('aria-label') || '') !== want.ariaLabel) return false;
		if (want.className && (el.className || '') !== want.className) return false;
		return Math.abs(rect.left - want.left) < 2 &&
			Math.abs(rect.top - want.top) < 2 &&
			Math.abs(rect.width - want.width) < 2 &&
			Math.abs(rect.height - want.height) < 2;
	};

	const visited = new Set();
	const walk = (root) => {
		if (!root || visited.has(root) || !root.querySelectorAll) return null;
		visited.add(root);
		for (const el of root.querySelectorAll('*')) {
			const sr = el.fakeShadowRoot || el.shadowRoot;
			if (sr) {
				const found = walk(sr);
				if (found) return found;
			}
			if (!visible(el)) continue;
			if (matches(el)) return el;
		}
		return null;
	};

	const found = walk(document);
	if (!found || typeof found.focus !== 'function') return false;
	found.focus();
	return true;
})()`

// FocusTargetScript renders focusTargetScriptTemplate for a specific target.
func FocusTargetScript(target ClickTarget) string {
	return fmt.Sprintf(focusTargetScriptTemplate,
		target.Tag, target.ID, target.Name, target.Title, target.Role,
		target.AriaLabel, target.ClassName,
		target.Left, target.Top, target.Width, target.Height)
}

// FocusHelperScript plants a zero-size button at the top-left of the viewport
// and focuses it, so the next Tab key press starts from a known position.
const FocusHelperScript = `(() => {
	let el = document.getElementById('__flaresolverr-focus');
	if (!el) {
		el = document.createElement('button');
		el.id = '__flaresolverr-focus';
		el.style.position = 'fixed';
		el.style.top = '0';
		el.style.left = '0';
		document.body.prepend(el);
	}
	el.focus();
	return true;
})()`

// ActiveElementScript describes document.activeElement for debug logging.
const ActiveElementScript = `(() => {
	const el = document.activeElement;
	if (!el) return '';
	const bits = [el.tagName || '', el.id ? ('#' + el.id) : '', el.getAttribute ? (el.getAttribute('name') || '') : ''];
	return bits.filter(Boolean).join(' ');
})()`

// HasFocusScript reports whether the document currently holds focus.
const HasFocusScript = `document.hasFocus ? document.hasFocus() : true`

// DocumentReadyScript reports whether the page has finished parsing.
//
// document.body is checked alongside readyState because a navigation that has
// been triggered but not yet committed still reports the *previous* document's
// "complete" — which is exactly the state right after a challenge clears.
const DocumentReadyScript = `document.readyState === 'complete' && !!document.body`
