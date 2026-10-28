// 无外部依赖，便于守卫测试在没有数据库环境时直接导入。
/** 文件池目录：规范绝对路径（无空段、`.`、`..`、尾部斜杠），且不在 /dev、/proc、/sys 下。 */
export function isValidPoolDirectory(directory: string): boolean {
  if (!/^\/[A-Za-z0-9_.\/-]+$/.test(directory) || directory.length > 255 || directory.endsWith('/')) return false
  if (directory.split('/').slice(1).some(seg => seg === '' || seg === '.' || seg === '..')) return false
  return !/^\/(dev|proc|sys)(\/|$)/.test(directory)
}
