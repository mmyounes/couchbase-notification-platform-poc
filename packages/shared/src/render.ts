// `{{param}}` substitution. Deliberately dependency-free.

const PARAM = /\{\{(\w+)\}\}/g

export class MissingTemplateParam extends Error {
  constructor (public readonly param: string) {
    super(`template parameter '${param}' not supplied`)
    this.name = 'MissingTemplateParam'
  }
}

/**
 * A missing parameter is an error, not an empty string. Silently rendering
 * "Your temporary password is ." would be worse than failing.
 */
export function render (tpl: string, params: Record<string, string | number>): string {
  let missing: string | null = null
  const out = tpl.replace(PARAM, (_m, key: string) => {
    if (!(key in params)) { missing ??= key; return '' }
    return String(params[key])
  })
  if (missing) throw new MissingTemplateParam(missing)
  return out
}
