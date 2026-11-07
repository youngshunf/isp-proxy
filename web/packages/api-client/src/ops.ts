// 由 OpenAPI 生成类型推导任意操作的入参与结果（两端共用）。
// 入参统一为一个对象：路径参数平铺，另加 query、body、idempotencyKey（契约要求 Idempotency-Key 头的操作才有）。
import type { operations } from './generated/schema'

export type Op = keyof operations

type Params<O extends Op> = operations[O]['parameters']

type PathPart<O extends Op> = Params<O> extends { path: infer P } ? (P extends object ? P : {}) : {}

type QueryPart<O extends Op> = Params<O> extends { query: infer Q }
  ? { query: Q }
  : Params<O> extends { query?: infer Q }
    ? [NonNullable<Q>] extends [never]
      ? {}
      : { query?: NonNullable<Q> }
    : {}

type IdemPart<O extends Op> = Params<O> extends { header: infer H }
  ? 'Idempotency-Key' extends keyof H
    ? {
        /** 每次用户意图生成一次（如打开对话框时），重试时复用；同一键不同请求体返回 409。 */
        idempotencyKey: string
      }
    : {}
  : {}

type BodyPart<O extends Op> = operations[O] extends { requestBody: { content: { 'application/json': infer B } } }
  ? { body: B }
  : operations[O] extends { requestBody?: { content: { 'application/json': infer B } } }
    ? [B] extends [never]
      ? {}
      : unknown extends B
        ? {}
        : { body?: B }
    : {}

type Simplify<T> = { [K in keyof T]: T[K] } & {}

/** 一个操作的入参：路径参数 + query + body + 幂等键。 */
export type OpInput<O extends Op> = Simplify<PathPart<O> & QueryPart<O> & BodyPart<O> & IdemPart<O>>

type JsonOf<R> = R extends { content: { 'application/json': infer T } } ? T : never

/** 一个操作的成功响应体（204 为 void）。 */
export type OpResult<O extends Op> = operations[O]['responses'] extends infer R
  ? 200 extends keyof R
    ? JsonOf<R[200]>
    : 201 extends keyof R
      ? JsonOf<R[201]>
      : 202 extends keyof R
        ? JsonOf<R[202]>
        : void
  : never

export type OpMethod<O extends Op> = {} extends OpInput<O>
  ? (input?: OpInput<O>) => Promise<OpResult<O>>
  : (input: OpInput<O>) => Promise<OpResult<O>>
