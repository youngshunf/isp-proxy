// jsdom 里补齐 antd 用到的浏览器接口；每个用例结束后卸载组件。
import { cleanup } from '@testing-library/react'
import { afterEach } from 'vitest'

afterEach(() => {
  cleanup()
  try {
    localStorage.clear()
  } catch {
    /* 忽略 */
  }
})

if (!window.matchMedia) {
  Object.defineProperty(window, 'matchMedia', {
    writable: true,
    value: (query: string) => ({
      matches: false,
      media: query,
      onchange: null,
      addListener: () => {},
      removeListener: () => {},
      addEventListener: () => {},
      removeEventListener: () => {},
      dispatchEvent: () => false,
    }),
  })
}

if (!('ResizeObserver' in window)) {
  class RO {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
  Object.defineProperty(window, 'ResizeObserver', { writable: true, value: RO })
  Object.defineProperty(globalThis, 'ResizeObserver', { writable: true, value: RO })
}

// antd 的部分组件读取 getComputedStyle 的伪元素参数，jsdom 不支持第二个参数时会打印警告。
const gcs = window.getComputedStyle.bind(window)
window.getComputedStyle = (el: Element) => gcs(el)

if (!navigator.clipboard) {
  Object.defineProperty(navigator, 'clipboard', { value: { writeText: async () => {} }, configurable: true })
}

window.scrollTo = () => {}
