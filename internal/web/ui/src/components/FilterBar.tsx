import type { Facet } from "../types";
import { LEVELS, includesTerm } from "../query";
import { fmtNum } from "../format";

interface Props {
  levels: Facet[];
  services: Facet[];
  query: string;
  wrap: boolean;
  onToggleTerm: (term: string) => void;
  onToggleWrap: () => void;
}

/**
 * Level and service counts, formerly a 270px sidebar of bars. The same
 * information as toggle chips, and the log lines get the full width.
 */
export function FilterBar({ levels, services, query, wrap, onToggleTerm, onToggleWrap }: Props) {
  const levelMap = new Map(levels.map((f) => [f.value, f.count]));
  const chip = (name: string, count: number, term: string, color?: string) => (
    <button
      key={term}
      className={`lvl-chip${includesTerm(query, term) ? " is-on" : ""}`}
      onClick={() => onToggleTerm(term)}
    >
      {color && <i style={{ background: color }} />}
      {name}
      <b>{fmtNum(count)}</b>
    </button>
  );

  return (
    <div className="filterbar" id="filterbar">
      <span className="fb-label">Levels</span>
      {LEVELS.filter((l) => levelMap.has(l)).map((l) =>
        chip(l, levelMap.get(l)!, `level=${l}`, `var(--${l})`),
      )}
      <span className="fb-sep" />
      <span className="fb-label">Services</span>
      {services.slice(0, 5).map((f) => chip(f.value, f.count, `service=${f.value}`))}
      <span className="fb-spacer" />
      <button
        className={`chip${wrap ? " is-on" : ""}`}
        onClick={onToggleWrap}
        title="Wrap long messages"
      >
        Wrap
      </button>
    </div>
  );
}
