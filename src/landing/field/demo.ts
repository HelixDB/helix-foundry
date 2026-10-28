// What the field and the page tell each other: which Explorer record the demo
// has open, and when a product figure has its dots in place.
type Listener = (index: number) => void;
const listeners = new Set<Listener>();
let selected = -1;

export const demo = {
  get selected() {
    return selected;
  },
  select(index: number) {
    if (index === selected) return;
    selected = index;
    listeners.forEach((l) => l(index));
  },
  subscribe(l: Listener) {
    listeners.add(l);
    l(selected);
    return () => {
      listeners.delete(l);
    };
  },
};
