/**
 * 极简 tar.gz 打包器（只处理普通文件）。
 *
 * 为什么不用 `npm pack`：
 *   1. Windows 文件系统无法表达 Unix 权限位——`fs.chmod()` 是空操作（实测 mode 恒为 666），
 *      于是 npm pack 出来的 Linux / macOS 平台包里二进制是 644，装到 Linux 上无法执行。
 *   2. 在 Windows 上调 npm CLI 必须经 shell（npm 是 .cmd），Node 24 会给出 DEP0190 警告。
 *
 * 直接按 tar 格式写出并显式指定每个条目的 mode，两个问题都不存在，
 * 也不需要转发器的运行期 chmod 兜底去补救。
 *
 * 不写目录条目：解包端会自动创建父目录，npm pack 本身也只写文件条目。
 */

import fs from 'node:fs';
import path from 'node:path';
import zlib from 'node:zlib';

const BLOCK = 512;
const EXECUTABLE_MODE = 0o755;
const REGULAR_FILE_MODE = 0o644;

const NAME_OFFSET = 0;
const NAME_LENGTH = 100;
const MODE_OFFSET = 100;
const MODE_LENGTH = 8;
const UID_OFFSET = 108;
const GID_OFFSET = 116;
const SIZE_OFFSET = 124;
const SIZE_LENGTH = 12;
const MTIME_OFFSET = 136;
const MTIME_LENGTH = 12;
const CHECKSUM_OFFSET = 148;
const CHECKSUM_LENGTH = 8;
const TYPEFLAG_OFFSET = 156;
const MAGIC_OFFSET = 257;
const VERSION_OFFSET = 263;

/** 读取 tar 的八进制字段。 */
function readOctal(buffer, offset, length) {
  const text = buffer
    .subarray(offset, offset + length)
    .toString('latin1')
    .replace(/\0.*$/, '')
    .trim();
  const value = Number.parseInt(text, 8);
  return Number.isNaN(value) ? 0 : value;
}

/** 写入 tar 的传统八进制字段：length-1 位八进制 + NUL。 */
function writeOctal(header, offset, length, value) {
  header.write(value.toString(8).padStart(length - 1, '0'), offset, length - 1, 'latin1');
  header.writeUInt8(0, offset + length - 1);
}

function writeChecksum(header) {
  let sum = 0;
  for (let i = 0; i < BLOCK; i += 1) {
    // 计算校验和时，chksum 字段本身按 8 个空格计
    const inChecksumField = i >= CHECKSUM_OFFSET && i < CHECKSUM_OFFSET + CHECKSUM_LENGTH;
    sum += inChecksumField ? 0x20 : header[i];
  }
  header.write(sum.toString(8).padStart(6, '0'), CHECKSUM_OFFSET, 6, 'latin1');
  header.writeUInt8(0, CHECKSUM_OFFSET + 6);
  header.writeUInt8(0x20, CHECKSUM_OFFSET + 7);
}

function makeHeader(name, size, mode, mtime) {
  const header = Buffer.alloc(BLOCK);

  // ustar 的 name 字段只有 100 字节；本项目的条目名远短于此，超长直接报错而不是静默截断
  if (Buffer.byteLength(name, 'latin1') >= NAME_LENGTH) {
    throw new Error(`条目名超过 ${NAME_LENGTH} 字节，需要 ustar 前缀扩展：${name}`);
  }

  header.write(name, NAME_OFFSET, NAME_LENGTH, 'latin1');
  writeOctal(header, MODE_OFFSET, MODE_LENGTH, mode);
  writeOctal(header, UID_OFFSET, 8, 0);
  writeOctal(header, GID_OFFSET, 8, 0);
  writeOctal(header, SIZE_OFFSET, SIZE_LENGTH, size);
  writeOctal(header, MTIME_OFFSET, MTIME_LENGTH, mtime);
  header.write('        ', CHECKSUM_OFFSET, CHECKSUM_LENGTH, 'latin1'); // 先占位，随后计算
  header.writeUInt8(0x30, TYPEFLAG_OFFSET); // '0' = 普通文件
  header.write('ustar\u0000', MAGIC_OFFSET, 6, 'latin1');
  header.write('00', VERSION_OFFSET, 2, 'latin1');

  writeChecksum(header);
  return header;
}

function padding(contentLength) {
  const remainder = contentLength % BLOCK;
  return remainder === 0 ? Buffer.alloc(0) : Buffer.alloc(BLOCK - remainder);
}

/** 包的 bin/ 下、非 .exe 的条目应当可执行（平台包二进制与主包转发器都适用）。 */
export function isExecutableEntry(packagePath) {
  return packagePath.startsWith('package/bin/') && packagePath.endsWith('.exe') === false;
}

/** 递归收集普通文件，返回相对路径（POSIX 分隔符，已排序以保证产物可复现）。 */
function collectFiles(rootDir, relativeDir = '') {
  const absoluteDir = relativeDir === '' ? rootDir : path.join(rootDir, relativeDir);
  const found = [];

  for (const entry of fs.readdirSync(absoluteDir, { withFileTypes: true })) {
    const relative = relativeDir === '' ? entry.name : `${relativeDir}/${entry.name}`;
    if (entry.isDirectory()) {
      found.push(...collectFiles(rootDir, relative));
    } else if (entry.isFile()) {
      found.push(relative);
    }
  }
  return found.sort();
}

/**
 * 把 sourceDir 下的全部文件打成 npm 可发布的 tgz。
 * 条目前缀为 `package/`，bin/ 下的可执行文件写入 0755。
 */
export function createPackageTarball(sourceDir, destPath) {
  const files = collectFiles(sourceDir);
  if (files.length === 0) {
    throw new Error(`目录为空，无法打包：${sourceDir}`);
  }

  const mtime = Math.floor(Date.now() / 1000);
  const chunks = [];

  for (const relative of files) {
    const content = fs.readFileSync(path.join(sourceDir, relative));
    const name = `package/${relative}`;
    const mode = isExecutableEntry(name) ? EXECUTABLE_MODE : REGULAR_FILE_MODE;
    chunks.push(makeHeader(name, content.length, mode, mtime), content, padding(content.length));
  }

  // 两个全零块标志归档结束
  chunks.push(Buffer.alloc(BLOCK * 2));

  fs.mkdirSync(path.dirname(destPath), { recursive: true });
  fs.writeFileSync(destPath, zlib.gzipSync(Buffer.concat(chunks), { level: 9 }));

  return { files, tarballBytes: fs.statSync(destPath).size };
}

/**
 * 读取 tgz 里的条目，用于测试与排查。
 * 返回 [{ name, size, mode, typeflag }]。
 */
export function readTarballEntries(tgzPath) {
  const tar = zlib.gunzipSync(fs.readFileSync(tgzPath));
  const entries = [];

  let offset = 0;
  while (offset + BLOCK <= tar.length) {
    if (tar.subarray(offset, offset + BLOCK).every((byte) => byte === 0)) {
      break;
    }
    const name = tar
      .subarray(offset + NAME_OFFSET, offset + NAME_OFFSET + NAME_LENGTH)
      .toString('latin1')
      .replace(/\0.*$/, '');
    const size = readOctal(tar, offset + SIZE_OFFSET, SIZE_LENGTH);

    entries.push({
      name,
      size,
      mode: readOctal(tar, offset + MODE_OFFSET, MODE_LENGTH),
      typeflag: String.fromCharCode(tar[offset + TYPEFLAG_OFFSET]),
    });

    offset += BLOCK + Math.ceil(size / BLOCK) * BLOCK;
  }
  return entries;
}
