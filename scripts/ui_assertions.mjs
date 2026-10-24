export default function assertLayout(element) {
  const doc = element.ownerDocument;
  const active = [...doc.querySelectorAll("[data-view][aria-current=page]")];
  const panels = [...doc.querySelectorAll("[data-view-panel]")].filter(node => !node.hidden);
  if (active.length !== 1 || panels.length !== 1 || active[0].dataset.view !== panels[0].dataset.viewPanel) throw new Error("菜单选择与可见内容不一致");
  const viewport = doc.documentElement.clientWidth;
  if (doc.documentElement.scrollWidth > viewport + 1 || doc.body.scrollWidth > viewport + 1) throw new Error("页面存在水平溢出");
  const overflow = [...panels[0].querySelectorAll(".table-wrap, .cell-content, .log-details pre")].filter(node => node.getClientRects().length && node.scrollWidth > node.clientWidth + 1);
  if (overflow.length) throw new Error("日志文本或详情存在水平溢出");
  if (active[0].dataset.view === "settings") {
    if (!doc.querySelector("#dailyPlan #add") || doc.querySelector(".form-actions #add")) throw new Error("添加时刻应位于每日计划区域");
    const random = doc.getElementById("randomArithmetic").checked;
    if (doc.getElementById("fixedPromptFields").hidden !== random || doc.getElementById("randomPromptPreview").hidden === random) throw new Error("发送模式与当前展示内容不一致");
    if (doc.querySelector("#webhookUsername, #webhookPassword")) throw new Error("仍存在独立账号密码输入框");
    for (const [headingId, buttonId] of [["scheduleHeading", "add"], ["notificationHeading", "testNotification"]]) {
      const heading = doc.getElementById(headingId), button = doc.getElementById(buttonId);
      if (heading.closest(".section-heading") !== button.closest(".section-heading")) throw new Error("设置操作应位于对应标题栏");
      const title = heading.getBoundingClientRect(), action = button.getBoundingClientRect();
      if (Math.abs((title.top + title.bottom) / 2 - (action.top + action.bottom) / 2) > 1) throw new Error("设置标题与操作按钮没有居中对齐");
    }
    if (viewport > 680) {
      for (const ids of [["webhookURL", "webhookMethod", "webhookFormat"], ["webhookHeaders", "webhookBody"]]) {
        const tops = ids.map(id => doc.getElementById(id).getBoundingClientRect().top);
        if (Math.max(...tops) - Math.min(...tops) > 1) throw new Error("同一行的通知输入框没有对齐");
      }
    }
  }
  return {view: active[0].dataset.view, viewport, horizontalOverflow: false, checkedCells: panels[0].querySelectorAll("td").length};
}
