function object(value) {
  return value !== null && typeof value === 'object' && !Array.isArray(value) ? value : {}
}

function payload(attrs, direction) {
  const value = attrs[`langfuse.observation.${direction}`] ?? attrs[`${direction}.value`]
  try {
    return object(typeof value === 'string' ? JSON.parse(value) : value)
  } catch {
    return {}
  }
}

function entries(values) {
  return Object.entries(object(values))
}

export function readDecision(attrs = {}) {
  const input = payload(attrs, 'input')
  const output = payload(attrs, 'output')
  const { questions, state } = input
  const { answers, refusal } = output
  const endpoint = attrs['llmproxy.decision.endpoint']
  const systemOne = endpoint === '/v1/systemone' || state !== undefined ||
    (questions != null && !Array.isArray(questions)) || (answers != null && !Array.isArray(answers))
  const rows = []
  if (systemOne) {
    const questionMap = new Map(entries(questions))
    const answerMap = new Map(entries(answers))
    const keys = new Set([...questionMap.keys(), ...answerMap.keys()])
    for (const name of keys) rows.push({ name, question: questionMap.get(name), answer: answerMap.get(name) })
  } else {
    const questionList = Array.isArray(questions) ? questions : []
    const answerList = Array.isArray(answers) ? answers : []
    const names = new Map()
    answerList.forEach((answer, index) => {
      if (answer?.name) names.set(answer.name, index)
    })
    const namedMatches = new Set(questionList.filter(question => names.has(question?.name)).map(question => names.get(question.name)))
    const used = new Set()
    questionList.forEach((question, index) => {
      const namedIndex = names.get(question?.name)
      const positional = answerList[index]
      const match = namedIndex ?? (!namedMatches.has(index) && (!question?.name || !positional?.name) ? index : undefined)
      const answer = answerList[match]
      if (match !== undefined) used.add(match)
      rows.push({ name: question?.name || answer?.name, question, answer })
    })
    answerList.forEach((answer, index) => {
      if (!used.has(index)) rows.push({ name: answer?.name, answer })
    })
  }
  return { endpoint, systemOne, context: systemOne ? state : input.input, rows, refusal }
}

export function distribution(answer, question) {
  const probabilities = answer?.probabilities
  if (Array.isArray(probabilities)) {
    return probabilities.map(item => ({ value: item?.value, label: item?.label, probability: item?.probability }))
  }
  return entries(probabilities).map(([value, probability]) => ({
    value,
    label: Object.hasOwn(object(answer?.legend), value) ? answer.legend[value]
      : Array.isArray(question?.criteria) && /^\d+$/.test(value) ? question.criteria[Number(value)] : undefined,
    probability,
  }))
}
