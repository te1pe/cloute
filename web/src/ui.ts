// только textNode — без XSS

type Child = Node | string | null | undefined | false;

export function el<K extends keyof HTMLElementTagNameMap>(
  tag: K,
  props: Partial<HTMLElementTagNameMap[K]> & { class?: string } = {},
  ...children: Child[]
): HTMLElementTagNameMap[K] {
  const node = document.createElement(tag);
  const { class: className, ...rest } = props;
  if (className !== undefined) {
    node.className = className;
  }
  Object.assign(node, rest);
  for (const child of children) {
    if (child === null || child === undefined || child === false) {
      continue;
    }
    node.append(typeof child === 'string' ? document.createTextNode(child) : child);
  }
  return node;
}

export function mount(root: HTMLElement, ...children: Child[]): void {
  root.replaceChildren();
  for (const child of children) {
    if (child) {
      root.append(typeof child === 'string' ? document.createTextNode(child) : child);
    }
  }
}
