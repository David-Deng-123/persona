// @vitest-environment jsdom
import { act, useRef } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { onJumpToComment } from "../../lib/comment-marks";
import { textQuote, parseCommentBlock } from "../../lib/comments";
import { addQuote, getQuotes, removeQuote, setQuotes, withQuoteBlock } from "../../lib/drafts";
import { t } from "../../i18n";
import { GutterNumber, LineBox, useLinePick } from "../session/files/LineComment";
import { ChatActionsContext } from "./chat-actions";
import { CommentBar, placeCommentBar } from "./CommentBar";
import { CommentMarks } from "./CommentMarks";
import { QuoteBars } from "./Quote";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

function setInputValue(input: HTMLInputElement, value: string) {
  const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!;
  setter.call(input, value);
  input.dispatchEvent(new Event("input", { bubbles: true }));
}

function mockRangeRect(range: Range, rect: DOMRect) {
  range.getClientRects = () => [rect] as unknown as DOMRectList;
  range.getBoundingClientRect = () => rect;
}

describe("inline comment floating bar and compact composer chip (#11585)", () => {
  let host: HTMLDivElement;
  let root: Root;
  const draftKey = "m1/s1";

  beforeEach(() => {
    host = document.createElement("div");
    document.body.appendChild(host);
    root = createRoot(host);
    setQuotes(draftKey, []);
    Object.defineProperty(window, "innerWidth", { value: 1280, configurable: true });
    Object.defineProperty(window, "innerHeight", { value: 800, configurable: true });
    window.matchMedia = vi.fn().mockImplementation((query: string) => ({
      matches: false,
      media: query,
      addEventListener: () => {},
      removeEventListener: () => {},
      addListener: () => {},
      removeListener: () => {},
      dispatchEvent: () => false,
    }));
  });

  afterEach(() => {
    act(() => root.unmount());
    host.remove();
    document.getSelection()?.removeAllRanges();
    setQuotes(draftKey, []);
    vi.restoreAllMocks();
  });

  it("clamps floating bar position inside the viewport and flips above when near the bottom", () => {
    expect(placeCommentBar({ x: 300, top: 100, bottom: 120 }, 320, 44, 1280, 800)).toEqual({ left: 140, top: 128 });
    // near right edge: clamped to vw - w - 8
    expect(placeCommentBar({ x: 1260, top: 100, bottom: 120 }, 320, 44, 1280, 800)).toEqual({ left: 952, top: 128 });
    // near bottom edge: flips above anchor.top
    expect(placeCommentBar({ x: 300, top: 760, bottom: 780 }, 320, 44, 1280, 800)).toEqual({ left: 140, top: 708 });
  });

  it("saves with Enter or ✓, allows empty-note comments, and cancels with Esc leaving no comment", () => {
    const onSave = vi.fn();
    const onCancel = vi.fn();

    act(() => {
      root.render(<CommentBar anchor={{ x: 200, top: 100, bottom: 120 }} autoFocus onSave={onSave} onCancel={onCancel} />);
    });

    const input = document.querySelector<HTMLInputElement>("[data-comment-bar] input")!;
    expect(input).toBeTruthy();
    expect(input.placeholder).toBe(t("chat-comments.placeholder"));
    expect(document.activeElement).toBe(input);

    // Save via Enter with a note
    act(() => {
      setInputValue(input, "Check this edge case");
      input.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
    });
    expect(onSave).toHaveBeenCalledWith("Check this edge case");

    // Empty-note comment via ✓ button
    onSave.mockClear();
    act(() => {
      setInputValue(input, "");
      document.querySelector<HTMLButtonElement>("[data-comment-save]")!.click();
    });
    expect(onSave).toHaveBeenCalledWith("");

    // Esc cancels
    act(() => {
      input.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
    });
    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  it("docks cleanly at narrow widths (~390px) while showing number and context", () => {
    Object.defineProperty(window, "innerWidth", { value: 390, configurable: true });
    window.matchMedia = vi.fn().mockImplementation((query: string) => ({
      matches: query.includes("639px"),
      media: query,
      addEventListener: () => {},
      removeEventListener: () => {},
      addListener: () => {},
      removeListener: () => {},
      dispatchEvent: () => false,
    }));

    act(() => {
      root.render(
        <CommentBar
          anchor={{ x: 300, top: 200, bottom: 220 }}
          number={2}
          context="Retry with exponential backoff"
          onSave={() => {}}
          onCancel={() => {}}
        />,
      );
    });

    const bar = document.querySelector<HTMLElement>("[data-comment-bar]")!;
    expect(bar.dataset.docked).toBe("true");
    const ctx = bar.querySelector("[data-comment-bar-context]")!;
    expect(ctx.textContent).toContain("2");
    expect(ctx.textContent).toContain("Retry with exponential backoff");
  });

  it("keeps the composer to a single compact chip row even with 3+ comments, and popover supports jump and remove", () => {
    const id1 = addQuote(draftKey, { ...textQuote("first passage", undefined), comment: { body: "note one" } }, false);
    const id2 = addQuote(draftKey, { ...textQuote("second passage", undefined), comment: { body: "" } }, false);
    const id3 = addQuote(draftKey, { ...textQuote("third passage", undefined), comment: { body: "note three" } }, false);
    const jumped: string[] = [];
    const offJump = onJumpToComment((id) => {
      jumped.push(id);
      return true;
    });

    const renderBars = () => {
      const quotes = getQuotes(draftKey).list;
      act(() => {
        root.render(
          <QuoteBars
            draftKey={draftKey}
            quotes={quotes}
            onRemove={(id) => {
              removeQuote(draftKey, id);
              renderBars();
            }}
          />,
        );
      });
    };

    try {
      renderBars();
      // Only a single compact chip row — no stacked full comment cards
      expect(host.querySelectorAll("[data-comment-chip-row]")).toHaveLength(1);
      expect(host.querySelectorAll("[data-comment-card]")).toHaveLength(0);
      const chip = host.querySelector<HTMLButtonElement>("[data-comment-chip]")!;
      expect(chip.textContent).toContain(t("chat-comments.count", { count: 3 }));

      // Open popover
      act(() => chip.click());
      const popover = document.querySelector<HTMLElement>("[data-comment-popover]")!;
      expect(popover).toBeTruthy();
      const items = popover.querySelectorAll("[data-comment-item]");
      expect(items).toHaveLength(3);
      expect(items[0].textContent).toContain("1");
      expect(items[0].textContent).toContain("first passage");
      expect(items[0].textContent).toContain("note one");

      // Jump to comment 2
      act(() => popover.querySelector<HTMLButtonElement>('[data-comment-jump="2"]')!.click());
      expect(jumped).toEqual([id2]);
      expect(document.querySelector("[data-comment-popover]")).toBeNull();

      // Reopen popover and remove comment 1
      act(() => chip.click());
      act(() => document.querySelector<HTMLButtonElement>('[data-comment-remove="1"]')!.click());
      expect(getQuotes(draftKey).list.map((q) => q.id)).toEqual([id2, id3]);
      expect(host.querySelector<HTMLButtonElement>("[data-comment-chip]")!.textContent).toContain(t("chat-comments.count", { count: 2 }));
      expect(id1).toBeTruthy();
    } finally {
      offJump();
    }
  });

  it("opens the floating bar on selection, saves without focusing composer, and clicks pin to edit or delete in place", () => {
    const composerFocus = vi.fn();

    function Harness() {
      const scroller = useRef<HTMLDivElement>(null);
      return (
        <ChatActionsContext.Provider value={{ draftKey, title: "Test", canCompose: true, speaker: "Persona", images: () => [] }}>
          <div ref={scroller} style={{ position: "relative", height: 400 }}>
            <div>
              <div data-items="msg-1">
                <p>Alpha beta gamma delta</p>
              </div>
            </div>
            <CommentMarks scroller={scroller} />
          </div>
          <textarea data-testid="fake-composer" onFocus={composerFocus} />
        </ChatActionsContext.Provider>
      );
    }

    // Mock ResizeObserver & scrollTo for jsdom
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );

    act(() => root.render(<Harness />));

    const scroller = host.firstElementChild as HTMLDivElement;
    scroller.getBoundingClientRect = () => new DOMRect(0, 0, 800, 400);
    const textNode = host.querySelector("p")!.firstChild as Text;

    // Select "beta gamma"
    const range = document.createRange();
    range.setStart(textNode, 6);
    range.setEnd(textNode, 16);
    mockRangeRect(range, new DOMRect(60, 40, 80, 18));
    const origCreateRange = document.createRange.bind(document);
    vi.spyOn(document, "createRange").mockImplementation(() => {
      const r = origCreateRange();
      mockRangeRect(r, new DOMRect(60, 40, 80, 18));
      return r;
    });
    const sel = document.getSelection()!;
    sel.removeAllRanges();
    sel.addRange(range);

    // Trigger selectionchange + rAF
    vi.spyOn(window, "requestAnimationFrame").mockImplementation((cb) => {
      cb(0);
      return 1;
    });
    act(() => {
      document.dispatchEvent(new Event("selectionchange"));
    });

    // Floating bar appears at selection
    const bar = document.querySelector<HTMLElement>("[data-comment-bar]")!;
    expect(bar).toBeTruthy();
    const input = bar.querySelector("input")!;

    // Pressing C focuses the floating bar input
    act(() => {
      window.dispatchEvent(new KeyboardEvent("keydown", { key: "c", bubbles: true }));
    });
    expect(document.activeElement).toBe(input);

    // Type note and press Enter -> saves without focusing composer
    act(() => {
      setInputValue(input, "Clarify this");
      input.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
    });
    expect(document.querySelector("[data-comment-bar]")).toBeNull();
    expect(composerFocus).not.toHaveBeenCalled();
    expect(getQuotes(draftKey).list).toHaveLength(1);
    expect(getQuotes(draftKey).list[0].text).toBe("beta gamma");
    expect(getQuotes(draftKey).list[0].comment?.body).toBe("Clarify this");
    expect(getQuotes(draftKey).editing).toBeNull();

    // Numbered pin is rendered in the transcript
    const pin = host.querySelector<HTMLButtonElement>('[data-comment-pin="1"]')!;
    expect(pin).toBeTruthy();

    // Clicking the pin reopens the floating bar with existing note
    act(() => pin.click());
    const editInput = document.querySelector<HTMLInputElement>("[data-comment-bar] input")!;
    expect(editInput.value).toBe("Clarify this");

    // Edit in place and save
    act(() => {
      setInputValue(editInput, "Updated note");
      editInput.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
    });
    expect(getQuotes(draftKey).list[0].comment?.body).toBe("Updated note");

    // Reopen pin and delete in place
    act(() => pin.click());
    act(() => document.querySelector<HTMLButtonElement>("[data-comment-delete]")!.click());
    expect(getQuotes(draftKey).list).toHaveLength(0);
    expect(host.querySelector('[data-comment-pin="1"]')).toBeNull();

    vi.unstubAllGlobals();
  });

  it("reuses the floating bar for code line comments (LineBox)", () => {
    const onAdd = vi.fn();
    const onCancel = vi.fn();

    act(() => {
      root.render(
        <LineBox
          where="src/app.ts:12-14 · unstaged"
          anchor={{ x: 120, top: 200, bottom: 220 }}
          onAdd={onAdd}
          onCancel={onCancel}
        />,
      );
    });

    const bar = document.querySelector<HTMLElement>("[data-comment-bar]")!;
    expect(bar).toBeTruthy();
    expect(bar.querySelector("[data-comment-bar-context]")!.textContent).toContain("src/app.ts:12-14 · unstaged");

    const input = bar.querySelector("input")!;
    act(() => {
      setInputValue(input, "Extract helper");
      input.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
    });
    expect(onAdd).toHaveBeenCalledWith("Extract helper");
  });

  it("extends the picked line range on shift-click even while LineBox is already open", () => {
    function GutterHarness() {
      const pick = useLinePick(true);
      return (
        <div>
          {[1, 2, 3, 4, 5].map((n) => (
            <GutterNumber key={n} n={n} pick={pick} enabled />
          ))}
          {pick.range && (
            <LineBox
              where={`src/app.ts:${pick.range.from}-${pick.range.to}`}
              anchor={pick.anchor}
              onAdd={() => pick.clear()}
              onCancel={pick.clear}
            />
          )}
        </div>
      );
    }

    act(() => root.render(<GutterHarness />));

    const line2 = host.querySelector<HTMLButtonElement>('[data-gutter="2"]')!;
    const line5 = host.querySelector<HTMLButtonElement>('[data-gutter="5"]')!;

    // Pick line 2 first -> opens LineBox at 2-2
    act(() => {
      line2.dispatchEvent(new MouseEvent("pointerdown", { button: 0, bubbles: true }));
      window.dispatchEvent(new Event("pointerup"));
    });
    expect(document.querySelector("[data-comment-bar-context]")?.textContent).toContain("src/app.ts:2-2");

    // Shift-click line 5 while LineBox is open -> extends range to 2-5 instead of resetting startLine
    act(() => {
      line5.dispatchEvent(new MouseEvent("pointerdown", { button: 0, shiftKey: true, bubbles: true }));
      window.dispatchEvent(new Event("pointerup"));
    });
    expect(document.querySelector("[data-comment-bar-context]")?.textContent).toContain("src/app.ts:2-5");
  });

  it("lets Escape pass through when selection bar is unfocused and ignores Escape during IME composition", () => {
    const onCancel = vi.fn();
    const pageEscape = vi.fn();
    window.addEventListener("keydown", pageEscape);

    try {
      act(() => {
        root.render(<CommentBar anchor={{ x: 200, top: 100, bottom: 120 }} autoFocus={false} onSave={() => {}} onCancel={onCancel} />);
      });

      const input = document.querySelector<HTMLInputElement>("[data-comment-bar] input")!;
      expect(document.activeElement).not.toBe(input);

      // Unfocused selection bar: Escape passes through to the page handler and does not cancel
      act(() => {
        window.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true, cancelable: true }));
      });
      expect(pageEscape).toHaveBeenCalledTimes(1);
      expect(onCancel).not.toHaveBeenCalled();

      // Focused bar during IME composition: Escape does not close the bar or lose input
      act(() => {
        input.focus();
        setInputValue(input, "nihon");
        const imeEsc = new KeyboardEvent("keydown", { key: "Escape", bubbles: true, cancelable: true });
        Object.defineProperty(imeEsc, "isComposing", { value: true });
        input.dispatchEvent(imeEsc);
      });
      expect(onCancel).not.toHaveBeenCalled();
      expect(input.value).toBe("nihon");

      // Focused bar with normal Escape: intercepts and cancels without reaching page listener
      pageEscape.mockClear();
      act(() => {
        input.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true, cancelable: true }));
      });
      expect(onCancel).toHaveBeenCalledTimes(1);
      expect(pageEscape).not.toHaveBeenCalled();
    } finally {
      window.removeEventListener("keydown", pageEscape);
    }
  });

  it("edits a pinless comment from the CommentsChip popover and preserves the original note on cancel", () => {
    // Simulate a comment restored from a sent message (parseCommentBlock -> no transcript/gutter anchor)
    const restored = parseCommentBlock(["Comment 1", "Selected text:", "> restored passage", "", "original note"].join("\n"))!;
    expect(restored.comment?.anchor).toBeUndefined();
    const id = addQuote(draftKey, restored, false);

    const renderBars = () => {
      const quotes = getQuotes(draftKey).list;
      act(() => {
        root.render(
          <QuoteBars
            draftKey={draftKey}
            quotes={quotes}
            onRemove={(qid) => {
              removeQuote(draftKey, qid);
              renderBars();
            }}
          />,
        );
      });
    };

    renderBars();
    const chip = host.querySelector<HTMLButtonElement>("[data-comment-chip]")!;

    // Open popover and click edit -> opens CommentBar prefilled with original note
    act(() => chip.click());
    const editBtn = document.querySelector<HTMLButtonElement>('[data-comment-edit="1"]')!;
    expect(editBtn).toBeTruthy();
    act(() => editBtn.click());

    const input = document.querySelector<HTMLInputElement>("[data-comment-bar] input")!;
    expect(input.value).toBe("original note");

    // Modify and cancel with Esc -> stored note stays unchanged
    act(() => {
      setInputValue(input, "discarded edit");
      input.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
    });
    expect(document.querySelector("[data-comment-bar]")).toBeNull();
    expect(getQuotes(draftKey).list.find((q) => q.id === id)?.comment?.body).toBe("original note");

    // Reopen popover, edit again, and save with Enter -> stored note updates
    act(() => chip.click());
    act(() => document.querySelector<HTMLButtonElement>('[data-comment-edit="1"]')!.click());
    const input2 = document.querySelector<HTMLInputElement>("[data-comment-bar] input")!;
    act(() => {
      setInputValue(input2, "updated before resend");
      input2.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
    });
    expect(document.querySelector("[data-comment-bar]")).toBeNull();
    expect(getQuotes(draftKey).list.find((q) => q.id === id)?.comment?.body).toBe("updated before resend");
  });

  it("keeps outgoing serialized message format unchanged", () => {
    const q1 = textQuote("first quote", undefined);
    q1.comment!.body = "first note";
    const q2 = textQuote("second quote", undefined);
    // empty note allowed
    expect(withQuoteBlock([q1, q2], "Please review")).toBe(
      [
        "> Comment 1",
        "> Selected text:",
        "> > first quote",
        "> ",
        "> first note",
        "",
        "> Comment 2",
        "> Selected text:",
        "> > second quote",
        "",
        "Please review",
      ].join("\n"),
    );
  });
});
