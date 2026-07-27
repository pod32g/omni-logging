import { useEffect, useMemo, useRef, useState } from "react";

export interface Command {
  kind: string;
  label: string;
  run: () => void;
}

interface Props {
  open: boolean;
  commands: Command[];
  onClose: () => void;
  /** Offered as "Search for …" so the palette doubles as a query launcher. */
  onSearch: (term: string) => void;
}

export function CommandPalette({ open, commands, onClose, onSearch }: Props) {
  const [term, setTerm] = useState("");
  const [sel, setSel] = useState(0);
  const inputRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (open) {
      setTerm("");
      setSel(0);
      inputRef.current?.focus();
    }
  }, [open]);

  const items = useMemo(() => {
    const t = term.trim().toLowerCase();
    const matched = commands.filter((c) => !t || c.label.toLowerCase().includes(t));
    if (!t) return matched;
    return [
      { kind: "search", label: `Search for “${term.trim()}”`, run: () => onSearch(term.trim()) },
      ...matched,
    ];
  }, [term, commands, onSearch]);

  if (!open) return null;

  const choose = (i: number) => {
    const it = items[i];
    if (!it) return;
    onClose();
    it.run();
  };

  return (
    <div
      className="palette-scrim"
      id="palette"
      onClick={(e) => {
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <div className="palette">
        <input
          id="pal-input"
          ref={inputRef}
          value={term}
          placeholder="Jump to a view, filter a level, or search…"
          autoComplete="off"
          spellCheck={false}
          onChange={(e) => {
            setTerm(e.target.value);
            setSel(0);
          }}
          onKeyDown={(e) => {
            if (e.key === "ArrowDown") {
              e.preventDefault();
              setSel((s) => (s + 1) % Math.max(1, items.length));
            } else if (e.key === "ArrowUp") {
              e.preventDefault();
              setSel((s) => (s - 1 + items.length) % Math.max(1, items.length));
            } else if (e.key === "Enter") {
              e.preventDefault();
              choose(sel);
            } else if (e.key === "Escape") {
              e.preventDefault();
              onClose();
            }
          }}
        />
        <div className="pal-list" id="pal-list">
          {items.length === 0 && <div className="pal-empty">Nothing matches.</div>}
          {items.map((it, i) => (
            <div
              key={it.kind + it.label}
              className={`pal-item${i === sel ? " is-sel" : ""}`}
              onMouseEnter={() => setSel(i)}
              onClick={() => choose(i)}
            >
              <span className="pal-kind">{it.kind}</span>
              <span className="pal-label">{it.label}</span>
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}
