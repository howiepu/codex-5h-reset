export default function assertLayout(element) {
  const doc = element.ownerDocument;
  const active = [...doc.querySelectorAll("[data-view][aria-current=page]")];
  const panels = [...doc.querySelectorAll("[data-view-panel]")].filter(node => !node.hidden);
  if (active.length !== 1 || panels.length !== 1 || active[0].dataset.view !== panels[0].dataset.viewPanel) throw new Error("菜单选择与可见内容不一致");
  const viewport = doc.documentElement.clientWidth;
  if (doc.documentElement.scrollWidth > viewport + 1 || doc.body.scrollWidth > viewport + 1) throw new Error("页面存在水平溢出");
  const overflow = [...panels[0].querySelectorAll(".table-wrap, .cell-content, .log-details pre")].filter(node => node.getClientRects().length && node.scrollWidth > node.clientWidth + 1);
  if (overflow.length) throw new Error("日志文本或详情存在水平溢出");
  return {view: active[0].dataset.view, viewport, horizontalOverflow: false, checkedCells: panels[0].querySelectorAll("td").length};
}
