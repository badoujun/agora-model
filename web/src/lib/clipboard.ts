/**
 * 复制文本到剪贴板。
 *
 * Clipboard API 只在「安全上下文」可用：HTTPS 或 localhost。Docker 部署后从
 * 其他机器用 `http://<内网 IP>:9090` 访问时，navigator.clipboard 是 undefined，
 * 直接调用会抛错（旧实现因此总是提示「复制失败」）。
 *
 * 这里优先走 Clipboard API，不可用或失败时退回 document.execCommand('copy')，
 * 返回是否真正复制成功，由调用方决定成功提示还是引导手动复制。
 */
export async function copyText(text: string): Promise<boolean> {
  if (window.isSecureContext && navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text)
      return true
    } catch {
      // 权限被拒或浏览器实现异常：继续尝试旧接口
    }
  }
  return copyByExecCommand(text)
}

/** 旧接口回退：临时 textarea + execCommand，兼容非安全上下文。 */
function copyByExecCommand(text: string): boolean {
  if (typeof document === 'undefined' || !document.body) return false

  const activeElement =
    document.activeElement instanceof HTMLElement ? document.activeElement : null
  const selection = document.getSelection()
  const previousRange = selection && selection.rangeCount > 0 ? selection.getRangeAt(0) : null

  const textarea = document.createElement('textarea')
  textarea.value = text
  // readonly：避免移动端弹出键盘；所有定位/样式都为了「不可见且不滚动页面」
  textarea.setAttribute('readonly', '')
  textarea.setAttribute('aria-hidden', 'true')
  textarea.style.position = 'fixed'
  textarea.style.top = '0'
  textarea.style.left = '0'
  textarea.style.width = '1px'
  textarea.style.height = '1px'
  textarea.style.padding = '0'
  textarea.style.border = 'none'
  textarea.style.outline = 'none'
  textarea.style.boxShadow = 'none'
  textarea.style.background = 'transparent'
  textarea.style.opacity = '0'

  document.body.appendChild(textarea)

  let ok = false
  try {
    textarea.focus({ preventScroll: true })
    textarea.select()
    // iOS Safari 只认 setSelectionRange 建立选区
    textarea.setSelectionRange(0, text.length)
    ok = document.execCommand('copy')
  } catch {
    ok = false
  } finally {
    document.body.removeChild(textarea)
  }

  // 还原用户原本的选区与焦点，复制动作不打断页面上的输入/选择
  if (selection) {
    selection.removeAllRanges()
    if (previousRange) selection.addRange(previousRange)
  }
  activeElement?.focus({ preventScroll: true })

  return ok
}
