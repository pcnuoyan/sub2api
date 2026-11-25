import DOMPurify from 'dompurify'

const TAGS = ['div', 'span', 'p', 'br', 'h1', 'h2', 'h3', 'style', 'svg', 'g', 'defs', 'symbol', 'use', 'path', 'rect', 'circle', 'ellipse', 'line', 'polyline', 'polygon', 'text', 'tspan', 'title', 'desc', 'clippath', 'mask', 'lineargradient', 'radialgradient', 'stop', 'pattern', 'animate', 'animatetransform', 'animatemotion', 'mpath']
const ATTRS = ['id', 'class', 'style', 'xmlns', 'viewBox', 'width', 'height', 'x', 'y', 'x1', 'y1', 'x2', 'y2', 'cx', 'cy', 'r', 'rx', 'ry', 'd', 'points', 'fill', 'fill-opacity', 'fill-rule', 'stroke', 'stroke-width', 'stroke-linecap', 'stroke-linejoin', 'stroke-dasharray', 'stroke-dashoffset', 'stroke-opacity', 'opacity', 'transform', 'transform-origin', 'clip-path', 'mask', 'offset', 'stop-color', 'stop-opacity', 'gradientUnits', 'gradientTransform', 'patternUnits', 'patternTransform', 'preserveAspectRatio', 'text-anchor', 'font-size', 'font-family', 'font-weight', 'dominant-baseline', 'href', 'xlink:href', 'attributeName', 'type', 'from', 'to', 'by', 'values', 'dur', 'begin', 'end', 'repeatCount', 'repeatDur', 'keyTimes', 'keySplines', 'keyPoints', 'calcMode', 'additive', 'accumulate', 'rotate', 'path']
const ANIMATED_ATTRIBUTES = new Set(['x', 'y', 'x1', 'x2', 'y1', 'y2', 'cx', 'cy', 'r', 'rx', 'ry', 'd', 'points', 'width', 'height', 'transform', 'opacity', 'fill', 'fill-opacity', 'stroke', 'stroke-width', 'stroke-dashoffset', 'stroke-opacity'])
export const MODEL_TEST_PREVIEW_CSP = "default-src 'none'; script-src 'none'; style-src 'unsafe-inline'; img-src 'none'; font-src 'none'; connect-src 'none'; media-src 'none'; object-src 'none'; frame-src 'none'; child-src 'none'; worker-src 'none'; form-action 'none'; base-uri 'none'"

export function buildModelTestPreview(source: string): string | null {
  const clean = DOMPurify.sanitize(source.replace(/```(?:html|xml|svg)?\s*\n?/gi, '').replace(/```/g, ''), {
    ALLOWED_TAGS: [...TAGS, 'html', 'head', 'body'],
    ALLOWED_ATTR: ATTRS,
    WHOLE_DOCUMENT: true,
    ALLOW_DATA_ATTR: false,
    ALLOW_ARIA_ATTR: false,
    ADD_TAGS: ['animate', 'animateTransform', 'animateMotion'],
    FORBID_TAGS: ['script', 'foreignObject', 'iframe', 'object', 'embed', 'form', 'input', 'button', 'a', 'meta', 'link', 'base', 'image', 'img', 'audio', 'video'],
    FORBID_ATTR: ['src', 'srcset', 'action', 'formaction', 'target']
  })
  const document = new DOMParser().parseFromString(clean, 'text/html')
  for (const element of document.body.querySelectorAll('*')) {
    for (const name of ['href', 'xlink:href']) {
      const value = element.getAttribute(name)
      if (value && !/^#[A-Za-z_][\w:.-]*$/.test(value)) element.removeAttribute(name)
    }
    if (element.localName.toLowerCase() === 'animate') {
      if (!ANIMATED_ATTRIBUTES.has((element.getAttribute('attributeName') ?? '').toLowerCase())) element.remove()
    }
  }
  // CSS 只负责隔离框内的画面；CSP 在任何样式之前禁止外部资源和所有脚本。
  if (!document.body.querySelector('svg path, svg rect, svg circle, svg ellipse, svg line, svg polyline, svg polygon, svg text, svg use')) return null
  const headStyles = [...document.head.querySelectorAll('style')].map(style => style.outerHTML).join('')
  return `<!doctype html><html><head><meta http-equiv="Content-Security-Policy" content="${MODEL_TEST_PREVIEW_CSP}"><meta name="referrer" content="no-referrer"><style>html,body{margin:0;min-height:100%;background:#fff;color:#111}body{overflow:auto}svg{max-width:100%;height:auto}</style>${headStyles}</head><body>${document.body.innerHTML}</body></html>`
}
